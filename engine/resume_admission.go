package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

func (e *Engine) admitAgentReply(ctx context.Context, in ResumeInput, paused *suspension.Suspension, snapshot *run.Run, first *suspension.Progress) (*suspension.Suspension, error) {
	durable, ok := e.store.(suspension.ResumeAdmissionStore)
	if !ok {
		return nil, suspension.ErrResumeAdmissionUnavailable
	}
	if paused == nil || in.agentReplyVersion == nil {
		return nil, suspension.ErrResumeAdmissionConflict
	}
	if err := checkResumeRecoveryAuthority(ctx, paused.RunID); err != nil {
		return nil, err
	}
	data, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	request := suspension.ResumeAdmissionRequest{Scope: paused.Scope, Version: *in.agentReplyVersion, Input: data}
	var admitted suspension.ResumeAdmission
	if claims, leased := e.leasedClaimsFor(snapshot); leased && first != nil {
		// The admission takes this worker's lease and records the reply as
		// done with it: an admitter that dies before it starts the resume
		// leaves a lease that lapses, and the sweep reclaims the run with
		// the reply, whether or not the host runs reply recovery.
		lease := e.newLease()
		first.Owner = lease.Owner
		admitted, err = claims.AdmitResumeLeased(ctx, request, lease, first)
	} else {
		admitted, err = durable.AdmitResume(ctx, request)
	}
	if err != nil {
		return nil, err
	}
	if admitted.Request.Version != request.Version || admitted.Request.Scope.Canonical() != request.Scope.Canonical() || !bytes.Equal(admitted.Request.Input, data) {
		return nil, suspension.ErrResumeAdmissionConflict
	}
	if err := checkResumeRecoveryAuthority(ctx, paused.RunID); err != nil {
		return nil, err
	}
	return e.startAdmittedResume(ctx, durable, admitted, snapshot, first)
}

// startAdmittedResume grants the admitted resume its one execution. When
// this engine leases the run, the start takes the lease for this worker,
// from whoever held it since the admission, and writes first (the reply
// recorded as done) in the same transaction: the one worker allowed to
// execute the reply is the one holding the run, and a worker that dies
// right after leaves a lease that lapses and the answer.
func (e *Engine) startAdmittedResume(ctx context.Context, durable suspension.ResumeAdmissionStore, admitted suspension.ResumeAdmission, snapshot *run.Run, first *suspension.Progress) (*suspension.Suspension, error) {
	var started suspension.ResumeStart
	var err error
	if claims, leased := e.leasedClaimsFor(snapshot); leased && first != nil {
		lease := e.newLease()
		first.Owner = lease.Owner
		started, err = claims.StartResumeLeased(ctx, admitted.Request.Version.RunID, admitted.Request.Version.SuspensionID, admitted.RequestDigest, lease, first)
	} else {
		started, err = durable.StartResume(ctx, admitted.Request.Version.RunID, admitted.Request.Version.SuspensionID, admitted.RequestDigest)
	}
	if err != nil {
		return nil, err
	}
	if !started.Fresh {
		return nil, suspension.ErrResumeAlreadyStarted
	}
	paused := started.Suspension
	if started.Version != admitted.Request.Version || started.Scope.Canonical() != admitted.Request.Scope.Canonical() || started.RequestDigest != admitted.RequestDigest || paused == nil || paused.ID != started.Version.SuspensionID || paused.RunID != started.Version.RunID || paused.Scope.Canonical() != started.Scope.Canonical() || paused.ClaimedRun == nil || paused.ClaimedRun.ID != paused.RunID || paused.ClaimedRun.Revision != admitted.ClaimedRevision || paused.ClaimedRun.State != run.StateRunning {
		return nil, suspension.ErrResumeAdmissionConflict
	}
	return paused, nil
}

type resumeRecoveryAuthorityKey struct{}
type resumeRecoveryAuthority struct {
	runID     id.AgentRunID
	authorize func(context.Context, id.AgentRunID) error
}

func checkResumeRecoveryAuthority(ctx context.Context, key id.AgentRunID) error {
	authority, ok := ctx.Value(resumeRecoveryAuthorityKey{}).(resumeRecoveryAuthority)
	if !ok {
		return nil
	}
	if authority.runID != key || authority.authorize == nil {
		return suspension.ErrResumeAdmissionUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return authority.authorize(ctx, key)
}

func (e *Engine) checkExecutionCall(ctx context.Context, key id.AgentRunID, provenance run.ExecutionProvenance) error {
	if err := checkResumeRecoveryAuthority(ctx, key); err != nil {
		return err
	}
	return e.checkTeamCall(ctx, key, provenance)
}

// RecoverAgentReply continues an admitted resume that has no execution start.
// The host callback checks current authority before reads, before starting and
// before every provider/tool call. An existing or ambiguous start cannot be
// replayed; ErrResumeAlreadyStarted requires inspection and reconciliation.
// This host API is not exposed as a tool or an unauthenticated transport route.
//
// The callback reads the principal on the context it is handed, and that
// changes once. The checks before the resume reads the run back run under
// ctx's principal, the recoverer's. The engine then restores the run's own
// principal, just before it admits or starts the resume, and every later
// check, including the one before each provider and tool call, runs under
// the run's principal.
func (e *Engine) RecoverAgentReply(ctx context.Context, runID id.AgentRunID, key id.SuspensionID, authorize func(context.Context, id.AgentRunID) error) (*run.Run, error) {
	if authorize == nil || runID.Prefix() != id.PrefixAgentRun || key.Prefix() != id.PrefixSuspension {
		return nil, suspension.ErrResumeAdmissionUnavailable
	}
	if _, err := cortex.RequireScope(ctx); err != nil {
		return nil, err
	}
	if e.llm == nil {
		return nil, cortex.ErrNoLLM
	}
	ctx = context.WithValue(ctx, resumeRecoveryAuthorityKey{}, resumeRecoveryAuthority{runID: runID, authorize: authorize})
	return e.traceResume(ctx, runID, resumeSourceLabel(resumeSourceAgentReply), func(tc context.Context) (*run.Run, error) {
		return e.recoverAgentReply(tc, runID, key)
	})
}

func (e *Engine) recoverAgentReply(ctx context.Context, runID id.AgentRunID, key id.SuspensionID) (*run.Run, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	if err := checkResumeRecoveryAuthority(ctx, runID); err != nil {
		return nil, err
	}
	current, readErr := e.store.GetRun(ctx, runID)
	if readErr != nil {
		return nil, readErr
	}
	var contextPolicy *cognitive.ContextPolicy
	if current.Provenance.ContextSelection != nil {
		if current.Provenance.ContextSelection.Validate() != nil {
			return nil, cortex.ErrInvalidContinuation
		}
		contextPolicy = &current.Provenance.ContextSelection.Policy
	}
	if contextErr := e.requireContextPolicy(contextPolicy); contextErr != nil {
		return nil, contextErr
	}
	var bindingErr error
	ctx, bindingErr = e.bindPreparedMessageRun(ctx, current)
	if bindingErr != nil {
		return nil, bindingErr
	}
	if runner, bound := ctx.Value(preparedMessagingKey{}).(*preparedTeamRunner); bound {
		if authorityErr := runner.authorizeMessagingCoordinator(ctx, runner.policy.ExecutionID); authorityErr != nil {
			return nil, authorityErr
		}
	}
	durable, ok := e.store.(suspension.ResumeAdmissionStore)
	if !ok {
		return nil, suspension.ErrResumeAdmissionUnavailable
	}
	admitted, err := durable.GetResumeAdmission(ctx, runID, key)
	if errors.Is(err, suspension.ErrResumeAdmissionNotFound) {
		return e.recoverUnadmittedReply(ctx, runID, key)
	}
	if err != nil {
		return nil, err
	}
	if admitted.Request.Version.RunID != runID || admitted.Request.Version.SuspensionID != key || admitted.Request.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return nil, suspension.ErrResumeAdmissionConflict
	}
	var in ResumeInput
	decoder := json.NewDecoder(bytes.NewReader(admitted.Request.Input))
	decoder.DisallowUnknownFields()
	if decodeErr := decoder.Decode(&in); decodeErr != nil {
		return nil, suspension.ErrResumeAdmissionConflict
	}
	in.agentReplyVersion = &admitted.Request.Version
	if authErr := checkResumeRecoveryAuthority(ctx, runID); authErr != nil {
		return nil, authErr
	}
	// The run read above is the pre-claim snapshot, and StartResume below
	// is the claim. claimForResume explains the order.
	ctx, revoked, err := e.restorePrincipal(ctx, current)
	if err != nil {
		return nil, err
	}
	// The reply the admission recorded goes into the first snapshot, so the
	// worker that starts the resume holds the run and its answer at once.
	// A pause that cannot be read now is a refusal before the start, so
	// the run is never started without its answer in the snapshot.
	pending, pendingErr := e.store.GetSuspension(ctx, runID)
	if pendingErr != nil {
		// A pause that is gone was most likely deleted by the resume that
		// already ran, and that is what the caller is told.
		if _, startErr := durable.GetResumeStart(ctx, runID, key); startErr == nil {
			return nil, suspension.ErrResumeAlreadyStarted
		}
		return nil, fmt.Errorf("load the pause of an admitted reply: %w", pendingErr)
	}
	first := claimSnapshot(pending, answeredCalls(pending.Pending, in))
	paused, err := e.startAdmittedResume(ctx, durable, admitted, current, first)
	if errors.Is(err, cortex.ErrSuspensionExpired) {
		return e.expireAdmittedReply(ctx, admitted)
	}
	if err != nil {
		return nil, err
	}
	if revoked != nil {
		return nil, e.failRevokedResume(ctx, current, paused, revoked)
	}
	if authErr := checkResumeRecoveryAuthority(ctx, runID); authErr != nil {
		return nil, e.failResume(ctx, paused.ClaimedRun, paused.ClaimedRun.AgentID, authErr)
	}
	ctx, err = e.leaseClaimedResume(ctx, current, paused, first)
	if err != nil {
		return nil, err
	}
	ctx, ready, err := e.rehydrateResume(ctx, in, resumeSourceAgentReply, paused.ClaimedRun, paused)
	if err != nil {
		return nil, err
	}
	return e.continueReAct(ctx, ready.ag, ready.cfg, ready.run, ready.state, ready.startedAt)
}

func (e *Engine) recoverUnadmittedReply(ctx context.Context, runID id.AgentRunID, key id.SuspensionID) (*run.Run, error) {
	paused, err := e.store.GetSuspension(ctx, runID)
	if err != nil {
		return nil, err
	}
	if paused.ID != key || paused.RunID != runID || paused.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() || paused.Reason != suspension.ReasonAgentReply || len(paused.Pending) == 0 || len(paused.Pending) > maxPendingQuestions {
		return nil, suspension.ErrResumeAdmissionConflict
	}
	reader, ok := e.store.(a2a.AskResolutionStore)
	if !ok {
		return nil, a2a.ErrAskResolutionUnavailable
	}
	resumer := a2aResumer{eng: e}
	bus := e.a2a
	if runner, bound := ctx.Value(preparedMessagingKey{}).(*preparedTeamRunner); bound {
		var readerErr error
		reader, readerErr = runner.resolutionReader()
		if readerErr != nil {
			return nil, readerErr
		}
		resumer.team, resumer.reader = runner.policy.ExecutionID, reader
		bus, readerErr = runner.messagingBus()
		if readerErr != nil {
			return nil, readerErr
		}
	}
	if releaseErr := e.releasePendingQuestions(ctx, paused, bus); releaseErr != nil {
		return nil, releaseErr
	}
	for _, call := range paused.Pending {
		if call.ReplyWith == "" {
			return nil, a2a.ErrAskResolutionConflict
		}
		resolution, readErr := reader.GetAskResolution(ctx, call.ReplyWith)
		if errors.Is(readErr, a2a.ErrAskResolutionNotFound) {
			continue
		}
		if readErr != nil {
			return nil, readErr
		}
		if resolution.Ask.AskerRunID != runID || resolution.Ask.ToolCallID != call.ID || resolution.Ask.ReplyWith != call.ReplyWith {
			return nil, a2a.ErrAskResolutionConflict
		}
		current, in, assembleErr := resumer.assembleResolvedQuestions(ctx, resolution, key)
		if assembleErr != nil || in == nil {
			return current, assembleErr
		}
		// RecoverAgentReply already owns the resume trace.
		return e.resumeRun(ctx, current.ID, *in, resumeSourceAgentReply)
	}
	return nil, suspension.ErrResumeAdmissionNotFound
}
