package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"go.opentelemetry.io/otel/attribute"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/telemetry"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
	"github.com/xraph/cortex/verification"
)

// ErrNotAgentReplyResumable is returned when something other than the
// messaging bus tries to answer a run that is waiting on a peer agent.
var ErrNotAgentReplyResumable = errors.New("cortex: run is waiting on an agent reply")

// ToolResult is one pending call's outcome, reported by whoever executed
// it while the run was paused.
//
// Error exists so a caller whose own execution failed can say so instead
// of dressing the failure up as a result. That is what keeps the
// one-terminal-event-per-call rule true across a pause: a resumed call
// emits ToolCompleted or ToolFailed, never both and never neither.
type ToolResult struct {
	// ToolCallID is the pending call's id, verbatim.
	ToolCallID string `json:"tool_call_id"`
	// Content is what the tool returned. It goes back to the model as
	// the tool message for this call.
	Content string `json:"content,omitempty"`
	// Error, when non-empty, means the call did not produce a result.
	// The model is told so in the same error payload shape an
	// engine-side tool failure produces, and Content is ignored.
	Error string `json:"error,omitempty"`
	// Execute says the call was never run and the engine should run it
	// now. It is what an approval produces: a call the authorizer
	// escalated was stopped before dispatch, so a human granting it hands
	// back permission, not a result, and somebody still has to execute
	// the tool.
	//
	// It exists so approval keeps the one-result-per-pending-call rule
	// rather than bending it. Content and Error are ignored when it is
	// set. Current authorization is checked again before dispatch; a fresh
	// denial still overrides the earlier approval.
	//
	// Only a tool the engine can run can be executed this way. An
	// external tool is answered by supplying its result, since the engine
	// is not the thing that runs it.
	Execute bool `json:"execute,omitempty"`
}

// ResumeInput is everything a caller supplies to continue a suspended
// run. One result per pending call, no more and no fewer.
type ResumeInput struct {
	ToolResults       []ToolResult `json:"tool_results"`
	agentReplyVersion *suspension.ClaimVersion
	// rejected says every result is a reviewer's rejection going back to
	// the model, so each call is recorded as denied rather than failed.
	// Only ContinueRun sets it, from a run made ready by a rejection.
	rejected bool
}

// resumption is the rehydrated run a claim produced, ready for whichever
// loop the caller asked for.
type resumption struct {
	ag        *agent.Config
	cfg       resolvedConfig
	run       *run.Run
	state     *reactState
	startedAt time.Time
	// paused is set when the run paused again before it reached the loop,
	// which only a reclaim does: there is no loop to run then.
	paused *pausedAgain
}

// Resume continues a suspended run synchronously once every pending call
// has a result.
//
// It answers an external-tool pause. A run paused for approval is not
// resumable this way: it is waiting on a decision, and the decision is
// what ResolveCheckpoint carries. Resuming one here is
// cortex.ErrRequiresApproval and the run stays paused.
//
// It re-derives authority from the STORED scope rather than the caller's,
// re-enters the loop at the step the suspension recorded, and restores
// the history boundary and the session from the continuation instead of
// recomputing either. Resuming with a different scope, a fresh session,
// or a rebuilt message list are three different ways to corrupt a run
// that looked fine at the call site and went wrong several turns later.
func (e *Engine) Resume(ctx context.Context, runID id.AgentRunID, in ResumeInput) (*run.Run, error) {
	return e.resume(ctx, runID, in, resumeSourcePublic)
}

// resumeSource is who is answering a paused run, which is the thing a
// public caller must not be able to claim for itself.
//
// It replaced an `approved bool` when agent-reply pauses arrived. Two
// callers with two authorities was expressible as a boolean; three is
// where a boolean starts lying, because "not approved" would have to mean
// both "an ordinary caller" and "the message bus", which are the two
// things that most need telling apart.
type resumeSource int

const (
	// resumeSourcePublic is an ordinary caller through Resume.
	resumeSourcePublic resumeSource = iota
	// resumeSourceApproval is a checkpoint decision: DecideCheckpoint or
	// ResolveCheckpoint having read a pending checkpoint for the run, or
	// ContinueRun carrying out a decision one of them recorded.
	resumeSourceApproval
	// resumeSourceAgentReply is the messaging bus, reaching here having
	// claimed the pending-ask row that proves the reply is genuine.
	resumeSourceAgentReply
)

// resume is Resume with the one thing a public caller must not be able to
// say: which authority this is arriving under.
//
// Every resume, public, approved or answered by a peer agent, is one run
// agent span marked resumed.
func (e *Engine) resume(ctx context.Context, runID id.AgentRunID, in ResumeInput, source resumeSource) (*run.Run, error) {
	return e.traceResume(ctx, runID, resumeSourceLabel(source), func(tc context.Context) (*run.Run, error) { return e.resumeRun(tc, runID, in, source) })
}

// traceResume is also where a synchronous resume is admitted, so Resume,
// ResolveCheckpoint, ContinueRun, the message bus's resumes and
// RecoverAgentReply all count against WithRunConcurrency and are refused
// while Stop drains. label is the span's resume source.
func (e *Engine) traceResume(ctx context.Context, runID id.AgentRunID, label string, action func(context.Context) (*run.Run, error)) (r *run.Run, err error) {
	ctx, done, err := e.admit(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	ctx, rt := e.startRun(ctx, modeSync, true,
		attribute.String(attrRunID, runID.String()),
		attribute.String(attrRunResumeSource, label))
	defer func() {
		if p := recover(); p != nil {
			rt.end(r, telemetry.ErrPanic)
			panic(p)
		}
		rt.end(r, err)
	}()
	return action(ctx)
}

func (e *Engine) resumeRun(ctx context.Context, runID id.AgentRunID, in ResumeInput, source resumeSource) (*run.Run, error) {
	ctx, rz, err := e.claimForResume(ctx, runID, in, source)
	if err != nil {
		return nil, err
	}
	return e.continueReAct(ctx, rz.ag, rz.cfg, rz.run, rz.state, rz.startedAt)
}

// ResumeStream is Resume over the streaming loop. The channel is closed
// when execution completes, same as StreamAgent's, and the run's admission
// slot is held until then.
func (e *Engine) ResumeStream(ctx context.Context, runID id.AgentRunID, in ResumeInput, events chan<- StreamEvent) (err error) {
	ctx, done, err := e.admit(ctx)
	if err != nil {
		close(events)
		return err
	}
	ctx, rt := e.startRun(ctx, modeStream, true,
		attribute.String(attrRunID, runID.String()),
		attribute.String(attrRunResumeSource, resumeSourceLabel(resumeSourcePublic)))
	defer func() {
		if p := recover(); p != nil {
			done()
			rt.end(nil, telemetry.ErrPanic)
			panic(p)
		}
		if err != nil {
			done()
			rt.end(nil, err)
		}
	}()
	ctx, rz, err := e.claimForResume(ctx, runID, in, resumeSourcePublic)
	if err != nil {
		close(events)
		return err
	}

	go e.streamResumed(ctx, rz, rt, events, done)
	return nil
}

// streamResumed runs a claimed resumption over the streaming loop on its
// own goroutine, closing events and releasing the admission slot when it
// ends.
func (e *Engine) streamResumed(ctx context.Context, rz *resumption, rt *runTrace, events chan<- StreamEvent, done func()) {
	defer close(events)
	defer done()
	defer func() {
		if p := recover(); p != nil {
			rt.end(rz.run, telemetry.ErrPanic)
			panic(p)
		}
		rt.end(rz.run, nil)
	}()

	// A resumed stream announces itself as started, because that is the
	// event every existing consumer already waits for before it will
	// render anything. A brand-new event type would be silently dropped by
	// all of them; the flag is there for the ones that draw a timeline and
	// care about the difference.
	sendEvent(ctx, events, StreamEvent{Type: EventRunStarted, Data: map[string]any{
		"run_id":   rz.run.ID.String(),
		"agent_id": rz.ag.ID.String(),
		"resumed":  true,
	}})
	if rz.paused != nil {
		// A reclaimed run that paused again before the loop: its consumer
		// hears the pause, as from the loop itself.
		sendEvent(ctx, events, StreamEvent{Type: EventSuspended, Data: map[string]any{
			"run_id":  rz.run.ID.String(),
			"reason":  string(rz.paused.reason),
			"pending": publicPendingCalls(rz.run, rz.paused.calls),
		}})
		return
	}

	e.continueStreamReAct(ctx, rz.ag, rz.cfg, rz.run, rz.state, rz.startedAt, events)
}

// claimForResume takes ownership of a paused run and rebuilds everything
// the loop needs to continue it. The returned context carries the run's
// own scope and the run's own principal, not the caller's.
//
// Once ClaimSuspension succeeds the run is running and it belongs to this
// call: every exit below either hands the loop a runnable state or fails
// the run. Returning an error with the run left running would be the
// wedge the claim's expiry guard exists to prevent, reached from the
// other side.
func (e *Engine) claimForResume(ctx context.Context, runID id.AgentRunID, in ResumeInput, source resumeSource) (context.Context, *resumption, error) {
	if e.store == nil {
		return nil, nil, cortex.ErrNoStore
	}
	if _, scopeErr := cortex.RequireScope(ctx); scopeErr != nil {
		return nil, nil, scopeErr
	}

	preview, previewErr := e.store.GetSuspension(ctx, runID)
	if previewErr != nil && in.agentReplyVersion != nil {
		return nil, nil, fmt.Errorf("read correlated suspension: %w", previewErr)
	}
	snapshot, err := e.previewResume(ctx, runID, in, source, preview, previewErr)
	if err != nil {
		return nil, nil, err
	}

	// The resumer's principal is replaced with the run's before the claim,
	// from the same pre-claim snapshot. A decode that fails for now leaves
	// the run paused; a revoked principal is held until the claim below
	// makes the run this call's to fail.
	ctx, revoked, err := e.restorePrincipal(ctx, snapshot)
	if err != nil {
		return nil, nil, err
	}

	var susp *suspension.Suspension
	var first *suspension.Progress
	if preview != nil && previewErr == nil {
		// What the run will have done once this claim lands: the state it
		// paused with and every call this resume answers. A leased claim
		// writes it with the claim, so a claimer that dies right after
		// leaves the answers for the worker that reclaims the run.
		first = claimSnapshot(preview, answeredCalls(preview.Pending, in))
	}
	leased := false
	if in.agentReplyVersion == nil {
		// Under this worker's lease, with the first snapshot, when it can.
		susp, leased, err = e.claimPauseLeased(ctx, snapshot, preview, time.Time{}, first)
	}
	switch versioned, supported := e.store.(suspension.VersionedStore); {
	case in.agentReplyVersion != nil:
		susp, err = e.admitAgentReply(ctx, in, preview, snapshot, first)
	case leased:
	case supported:
		if previewErr != nil {
			return nil, nil, fmt.Errorf("read suspension version: %w", previewErr)
		}
		if preview == nil {
			return nil, nil, cortex.ErrNotSuspended
		}
		susp, err = versioned.ClaimSuspensionVersion(ctx, suspension.ClaimVersion{RunID: runID, SuspensionID: preview.ID, RunRevision: snapshot.Revision})
	default:
		susp, err = e.store.ClaimSuspension(ctx, runID)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("claim suspension: %w", err)
	}
	if revoked != nil {
		return nil, nil, e.failRevokedResume(ctx, snapshot, susp, revoked)
	}
	if in.agentReplyVersion != nil {
		if authErr := checkResumeRecoveryAuthority(ctx, runID); authErr != nil {
			return nil, nil, e.failResume(ctx, susp.ClaimedRun, susp.ClaimedRun.AgentID, authErr)
		}
	}

	// A leased claim already holds the lease and the first snapshot. A
	// claim made the old way left the run running with no owner, and a
	// leased run takes its lease now, before anything of the resume runs.
	ctx, err = e.leaseClaimedResume(ctx, snapshot, susp, first)
	if err != nil {
		return nil, nil, err
	}
	return e.rehydrateResume(ctx, in, source, snapshot, susp)
}

// leaseClaimedResume holds the lease of a run just claimed from a pause,
// when this engine leases it. A claim that took this worker's lease wrote
// first with it, and the pass carries on from that snapshot (first nil
// reads it back). Otherwise the lease is taken now and the state the run
// paused with is saved as its first progress boundary, with first's
// answers.
//
// When the lease cannot be taken the pass ends with that error and the
// resume does nothing: see leaseClaimed.
func (e *Engine) leaseClaimedResume(ctx context.Context, snapshot *run.Run, susp *suspension.Suspension, first *suspension.Progress) (context.Context, error) {
	claimed := susp.ClaimedRun
	if claimed == nil {
		claimed = snapshot
	}
	if adopted, ok := e.adoptLease(ctx, claimed, first); ok {
		return adopted, nil
	}
	ctx, err := e.leaseClaimed(ctx, claimed)
	if err != nil {
		return ctx, err
	}
	if first == nil {
		first = claimSnapshot(susp, nil)
	}
	if err := e.saveProgress(ctx, susp.RunID, first); err != nil {
		return ctx, err
	}
	return ctx, nil
}

// pausedCont copies a suspension's continuation for a progress snapshot.
func pausedCont(cont suspension.Continuation) suspension.Continuation {
	cont.Messages = cloneExecutionMessages(cont.Messages)
	cont.ToolResults = slices.Clone(cont.ToolResults)
	return cont
}

// previewResume runs every check that can refuse a resume before anything
// moves, and returns the run it read. A refusal here leaves the run where
// it was, paused or ready, for the caller to try again.
//
// Every path that answers a paused run calls it before its first write:
// claimForResume before the claim, DecideCheckpoint and ProvideResults
// before MarkReady, ContinueRun before ClaimContinuation. That is what
// keeps an approval or an answer recorded for later from skipping a check
// a synchronous resume would have run. It checks, in order:
//
//   - an agent reply's binding to the exact pause it answers;
//   - the frozen context and publication policies, against this engine's;
//   - in the results, exactly one per pending call;
//   - that source may answer a pause of this reason;
//   - an agent reply's run revision;
//   - the context selection the run was prepared with;
//   - a prepared team run's orchestration binding;
//   - required verification, which also refuses a run whose suspension
//     could not be read.
//
// susp and suspErr are the caller's read of the suspension, since some
// callers need it before they can build in.
//
// The results are checked twice, and this first check is the one that is
// kind rather than correct. A caller that got the call ids wrong has its
// run left as it was and can retry, because nothing has moved yet.
// Checking only after the claim would fail the run over a malformed
// request, which is a cliff external-tool integrations would fall off
// constantly. The authoritative check is rehydrateResume's, against the
// suspension the claim actually returned.
func (e *Engine) previewResume(ctx context.Context, runID id.AgentRunID, in ResumeInput, source resumeSource, susp *suspension.Suspension, suspErr error) (*run.Run, error) {
	if bindingErr := checkAgentReplyVersion(susp, in, source); bindingErr != nil {
		return nil, bindingErr
	}
	if suspErr == nil {
		if _, snap, decodeErr := decodeContinuation(susp.Cont); decodeErr != nil {
			return nil, decodeErr
		} else if snap != nil {
			if contextErr := e.requireContextPolicy(snap.Config.Context); contextErr != nil {
				return nil, contextErr
			}
			if policyErr := e.requirePublication(snap.Config.Publication); policyErr != nil {
				return nil, policyErr
			}
		}
		if vErr := validateResults(susp.Pending, in.ToolResults); vErr != nil {
			if susp.Cont.Provenance.VerificationRequired || susp.Cont.SchemaVersion == requiredPreparedSchemaVersion || susp.Cont.SchemaVersion == requiredContextPreparedSchemaVersion {
				vErr = cortex.ErrResultsMismatch
			}
			return nil, vErr
		}
		if aErr := checkResumeAuthority(susp.Reason, source); aErr != nil {
			return nil, aErr
		}
	}

	// The run is read BEFORE any write, and a read that does not come
	// back ends the resume here. Nothing has moved at that point, and the
	// caller can try again. Reading only after the claim is what used to
	// strand a run on a store hiccup, since a claimed run nobody fails is
	// running forever.
	//
	// The caller's scope is enough for this read. A claim matches on an
	// exact scope, while this read may also serve an authorized ancestor.
	snapshot, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("load run to resume: %w", err)
	}
	if in.agentReplyVersion != nil && snapshot.Revision != in.agentReplyVersion.RunRevision {
		return nil, cortex.ErrRunConflict
	}
	if contextErr := e.checkContextResume(snapshot, susp, suspErr); contextErr != nil {
		return nil, contextErr
	}
	if bindingErr := e.checkPreparedTeamResume(ctx, snapshot, susp); bindingErr != nil {
		return nil, bindingErr
	}

	if suspErr == nil {
		if policyErr := e.checkRequiredResume(ctx, snapshot, susp); policyErr != nil {
			return nil, policyErr
		}
	} else if snapshot.Provenance.VerificationRequired || snapshot.Provenance.VerificationIntentID != "" || e.requiredVerification != nil {
		return nil, cortex.ErrVerificationUnavailable
	}
	return snapshot, nil
}

func (e *Engine) rehydrateResume(ctx context.Context, in ResumeInput, source resumeSource, snapshot *run.Run, susp *suspension.Suspension) (context.Context, *resumption, error) {
	runID := susp.RunID

	// The claim requires the run's exact scope. Retain its stored identity
	// for every resumed write and authorization check.
	ctx = cortex.WithScope(ctx, susp.Scope)

	r := e.claimedRun(ctx, runID, snapshot, susp.ClaimedRun)
	runTraceFrom(ctx).bind(r, susp.Cont.Config.Model)

	ag, err := e.store.Get(ctx, r.AgentID)
	if err != nil {
		return nil, nil, e.failResume(ctx, r, r.AgentID, fmt.Errorf("resolve agent: %w", err))
	}

	if err := validateResults(susp.Pending, in.ToolResults); err != nil {
		return nil, nil, e.failResume(ctx, r, ag.ID, err)
	}
	if err := checkResumeAuthority(susp.Reason, source); err != nil {
		return nil, nil, e.failResume(ctx, r, ag.ID, err)
	}

	if policyErr := e.checkRequiredResume(ctx, r, susp); policyErr != nil {
		return nil, nil, e.failResume(ctx, r, ag.ID, policyErr)
	}
	ctx, ag, st, resumeConfig, restoreErr := e.restoreLoopState(ctx, r, ag, susp.Cont)
	if restoreErr != nil {
		return nil, nil, restoreErr
	}
	if err := e.authorizeResumedCalls(ctx, r, susp, in.ToolResults, st); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			err = context.DeadlineExceeded
			if terminalErr := e.finalizeRun(ctx, r, run.Outcome{StopReason: run.StopDeadline, StepCount: st.stepIndex, TokensUsed: st.tokensUsed}); terminalErr != nil {
				err = errors.Join(err, terminalErr)
			}
			return nil, nil, err
		}
		safeErr := e.checkedToolError(ctx, cortex.Subject{AgentID: ag.ID, RunID: r.ID}, err)
		return nil, nil, e.failResume(ctx, r, ag.ID, safeErr)
	}

	var retained []llm.Message
	if r.Provenance.VerificationRequired {
		retained = cloneExecutionMessages(st.messages)
	}
	if checkErr := e.checkMessages(ctx, ag, r, resumeConfig.Publication, st.messages); checkErr != nil {
		return nil, nil, e.stopPublication(ctx, r, st, checkErr)
	}
	if r.Provenance.VerificationRequired && !reflect.DeepEqual(retained, st.messages) {
		return nil, nil, e.failResume(ctx, r, ag.ID, cortex.ErrVerificationUnavailable)
	}
	for _, pending := range susp.Pending {
		if checkErr := e.checkToolCalls(ctx, ag, r, resumeConfig.Publication, []llm.ToolCall{{ID: pending.ID, Name: pending.Name, Arguments: pending.Arguments}}); checkErr != nil {
			return nil, nil, e.stopPublication(ctx, r, st, checkErr)
		}
	}
	toolCtx, cancelTools := runDeadline(ctx, resumeConfig)
	resultErr := e.recordResumedCalls(toolCtx, ag, r, susp, in, st, resumeConfig)
	if interruption(ctx, r.ID) != nil {
		// Interrupted while it carried out the decision or the answers: a
		// result that came back afterwards is not recorded, and the run is
		// left or handed off from its last boundary.
		cancelTools()
		return nil, nil, leftResume(r, e.leaveRun(ctx, r, nil))
	}
	if errors.Is(resultErr, cortex.ErrEffectUncertain) {
		// Preserve an elapsed run deadline before releasing this context.
		// Cancelling it first would mislabel every uncertain action cancelled.
		terminalErr := e.finalizeUncertainEffect(toolCtx, r, st)
		cancelTools()
		return nil, nil, errors.Join(cortex.ErrEffectUncertain, terminalErr)
	}
	cancelTools()
	if resultErr != nil {
		return nil, nil, e.stopPublication(ctx, r, st, resultErr)
	}
	st.provenance = r.Provenance

	// The suspension goes before the loop re-enters, not after the run
	// finishes. One suspension per run is what makes ClaimSuspension a
	// meaningful primitive, so a run that suspends again a step later
	// would collide with its own stale row and fail on a write that
	// should have succeeded. A row that outlives its claim is also a lie
	// to every reader: it says paused about a run that is running.
	if err := e.store.DeleteSuspension(ctx, runID); err != nil {
		return nil, nil, e.failResume(ctx, r, ag.ID, fmt.Errorf("delete claimed suspension: %w", err))
	}

	// The config comes from the continuation, never from a fresh merge of
	// the agent's. A rebuild would drop the run's own overrides: cfg.Tools
	// feeds resolveTools, so a run deliberately started with a narrowed
	// tool list would resume with the agent's full set, which is exactly
	// the authority widening this whole path exists to refuse.
	cfg := configFromContinuation(susp.Cont.Config)
	if st.preparedSnapshot != nil {
		cfg.Verification = st.preparedSnapshot.Config.Verification
	}
	rz, entryErr := e.loopEntry(ctx, r, ag, st, cfg)
	if entryErr != nil {
		return nil, nil, entryErr
	}
	return ctx, rz, nil
}

// restoreLoopState rebuilds the loop state cont describes, and the context
// the loop runs under, for a run this call has claimed: the frozen
// snapshot and its revisions, the provenance, the team and messaging
// bindings, and the publication context. A resume rebuilds from the
// suspension it claimed, a reclaim from the run's saved progress. The
// returned config is the one the run's publication checks use.
//
// A refusal ends the run: it is failed, or stopped for publication, and
// the error says why.
func (e *Engine) restoreLoopState(ctx context.Context, r *run.Run, ag *agent.Config, cont suspension.Continuation) (context.Context, *agent.Config, *reactState, resolvedConfig, error) {
	prepared, snap, decodeErr := decodeContinuation(cont)
	if decodeErr != nil {
		return nil, nil, nil, resolvedConfig{}, e.failResume(ctx, r, ag.ID, decodeErr)
	}
	if err := validateFrozenContinuation(cont, snap, r); err != nil {
		return nil, nil, nil, resolvedConfig{}, e.failResume(ctx, r, ag.ID, err)
	}
	if bindingErr := e.checkOrchestrationBinding(ctx, r.ID, snap, r.Provenance, r.Budget); bindingErr != nil {
		return nil, nil, nil, resolvedConfig{}, e.failResume(ctx, r, ag.ID, bindingErr)
	}
	if snap != nil {
		if err := e.checkPreparedRevisions(snap); err != nil {
			return nil, nil, nil, resolvedConfig{}, e.failResume(ctx, r, ag.ID, err)
		}
		ag = &snap.Agent
	}
	st := stateFromContinuation(cont)
	st.prepared = prepared
	st.preparedSnapshot = snap
	if snap == nil {
		st.usage.Steps = st.stepIndex
		st.usage.Tokens = st.tokensUsed
		st.usage.ReportedTokens = st.tokensUsed
	}
	ctx = withExecutionProvenance(ctx, st.provenance)
	ctx = withTaskCallIndex(ctx, st.stepIndex-1)
	r.Provenance = st.provenance
	ctx = withTeamCallBinding(ctx, r.ID, snap, r.Provenance, r.Budget)
	if snapshotUsesMessaging(snap) {
		boundCtx, bindingErr := e.bindPreparedMessageRun(ctx, r)
		if bindingErr != nil {
			return nil, nil, nil, resolvedConfig{}, e.failResume(ctx, r, ag.ID, bindingErr)
		}
		ctx = boundCtx
	}
	r.Usage = st.usage
	if snap != nil {
		sourceStatus, checkErr := e.validatePreparedPublication(ctx, snap)
		if checkErr != nil {
			return nil, nil, nil, resolvedConfig{}, e.stopPublication(ctx, r, st, checkErr)
		}
		r.Provenance.PublicationStatus = mergePublicationStatus(r.Provenance.PublicationStatus, sourceStatus)
	}
	resumeConfig := configFromContinuation(cont.Config)
	if snap != nil {
		resumeConfig.Verification = snap.Config.Verification
	}
	if resumeConfig.Publication.Mode == "" {
		resumeConfig.Publication = e.frozenPublication()
	}
	ctx = withPublicationContext(ctx, ag, r, resumeConfig.Publication)
	return ctx, ag, st, resumeConfig, nil
}

// loopEntry is the last check before a claimed run re-enters the loop at
// st's step: the config must carry a step budget, and the budget must have
// room for that step. cfg is the continuation's own, never a fresh merge of
// the agent's: a rebuilt MaxSteps misfires the guard below.
func (e *Engine) loopEntry(ctx context.Context, r *run.Run, ag *agent.Config, st *reactState, cfg resolvedConfig) (*resumption, error) {
	// A continuation with no config at all is unusable, and it has to say
	// so in its own words. MaxSteps reads as 0, the guard just below
	// fires, and the run fails with ErrMaxStepsReached: an error that
	// means "this run used up its budget" and sends whoever reads it to
	// raise the budget, when nothing was ever going to run. Failing loud
	// is right; failing under somebody else's contract is not.
	if cfg.MaxSteps <= 0 {
		return nil, e.failResume(ctx, r, ag.ID,
			fmt.Errorf("resume run %s: continuation carries no step budget: %w", r.ID, cortex.ErrInvalidContinuation))
	}
	if st.stepIndex >= cfg.MaxSteps {
		// Suspending on the last step stores a step index the budget has
		// no room for. Re-entering the loop here would fall straight
		// through it and complete the run with empty output, which reads
		// as a model that had nothing to say rather than a run that ran
		// out of steps. The results the caller worked to produce are
		// already recorded and their terminal events have already fired,
		// so nothing is thrown away; what is refused is the pretence that
		// the run finished.
		limitErr := fmt.Errorf("resume run %s at step %d of %d: %w", r.ID, st.stepIndex, cfg.MaxSteps, cortex.ErrMaxStepsReached)
		if err := e.finalizeRun(ctx, r, run.Outcome{StopReason: run.StopStepLimit, StepCount: st.stepIndex, TokensUsed: st.tokensUsed}); err != nil {
			return nil, errors.Join(limitErr, err)
		}
		return nil, limitErr
	}

	// The run's own start, not this call's, so a resumed run reports the
	// wall-clock time it actually took rather than only the part after
	// the pause.
	startedAt := time.Now().UTC()
	if r.StartedAt != nil {
		startedAt = *r.StartedAt
	}
	return &resumption{ag: ag, cfg: cfg, run: r, state: st, startedAt: startedAt}, nil
}

// leftResume is the error a resume returns when its pass left the run:
// the hand-off or the lost lease, or, when the run turned out to have
// ended already, cortex.ErrRunLeaseLost, since this pass no longer owns it.
func leftResume(r *run.Run, err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("cortex: run %s ended while it was resumed: %w", r.ID, cortex.ErrRunLeaseLost)
}

// claimedRun uses the exact snapshot returned by a revision-aware atomic claim.
// Older custom stores retain their historical read/fallback behavior; they do
// not acquire the built-in stores' optimistic concurrency guarantee.
func (e *Engine) claimedRun(ctx context.Context, runID id.AgentRunID, snapshot, claimed *run.Run) *run.Run {
	if claimed != nil {
		return claimed
	}
	r, err := e.store.GetRun(ctx, runID)
	if err == nil {
		return r
	}
	e.logError("read a claimed run, falling back to the pre-claim snapshot", err,
		log.String("run_id", runID.String()),
	)
	// The claim moved the run to running; the snapshot predates it. A
	// later write of the snapshot as it was would put the run back to
	// paused under its own lease.
	fallback := *snapshot
	fallback.State = run.StateRunning
	return &fallback
}

// failResume marks a claimed run failed and hands the reason back
// unwrapped, so callers can still match it with errors.Is.
func (e *Engine) failResume(ctx context.Context, r *run.Run, agentID id.AgentID, err error) error {
	if r.Provenance.VerificationRequired {
		err = closedRequiredError(err)
	}
	e.failRun(ctx, r, agentID, err, time.Time{})
	return err
}

// recordResumedCalls closes the books on the calls the run paused for: a
// tool call row, exactly one terminal plugin event, and the tool message
// the model needs to see next to its own tool call.
//
// The terminal event is the part worth stating outright. v1.10.0
// established that every tool call fires exactly one of completed, failed
// or denied, and a pending call correctly fires none, because pending is
// not an ending. That only stays true if the resume supplies
// the missing one, so it does: a call the host executed emits
// ToolCompleted, a call the host could not execute emits ToolFailed. A
// tool call that suspends and then completes must not vanish from an
// audit trail, and one that suspends and then fails must not look like a
// success.
//
// A rejection that goes back to the model (in.rejected) is the one
// exception to completed or failed: the call was refused by a person, so
// it emits ToolDenied, the event an authorizer's refusal emits.
func (e *Engine) recordResumedCalls(ctx context.Context, ag *agent.Config, r *run.Run, susp *suspension.Suspension, in ResumeInput, st *reactState, cfg resolvedConfig) error {
	ctx = withOperationalRuntime(ctx, r, cfg, st)
	byID := make(map[string]ToolResult, len(in.ToolResults))
	for _, res := range in.ToolResults {
		byID[res.ToolCallID] = res
	}

	stepID := e.stepForPendingCalls(ctx, r.ID, susp.Cont.StepIndex)
	startedAt := susp.CreatedAt

	// The subject an approved call is dispatched under. It is the run's
	// own, rebuilt from the run rather than the resumer, for the same
	// reason the whole resume path re-derives scope from the suspension:
	// the scope is the suspension's, and the principal on ctx is the one
	// restorePrincipal decoded from the run before the claim.
	subject := cortex.Subject{
		Scope:     cortex.ScopeFromContext(ctx),
		Principal: cortex.PrincipalFromContext(ctx),
		AgentID:   r.AgentID,
		RunID:     r.ID,
	}

	// Pending order, not results order: the model sees its tool results
	// in the order it asked for them, and a caller is not obliged to
	// answer in that order.
	for _, p := range susp.Pending {
		// validateResults has already proven there is exactly one result
		// per pending id, so this lookup cannot miss.
		res := byID[p.ID]
		content, errText, outcome := e.resumedCallResult(ctx, subject, p, res, susp.Reason == suspension.ReasonApproval)
		if cause := interruption(ctx, r.ID); cause != nil {
			return cause
		}
		if outcome == outcomeUncertain {
			e.emitToolFailed(ctx, r.ID, p.Name, cortex.ErrEffectUncertain)
			return cortex.ErrEffectUncertain
		}
		approved, checkErr := e.checkToolResult(ctx, ag, r, cfg.Publication, content, "tool_result")
		if checkErr != nil {
			return checkErr
		}
		content = string(approved)
		if errText != "" {
			checkedError, scanErr := e.checkToolResult(ctx, ag, r, cfg.Publication, errText, "tool_error")
			if scanErr != nil {
				return scanErr
			}
			errText = string(checkedError)
			if errText == "" {
				errText = "tool execution failed"
			}
		}
		completedAt := time.Now().UTC()

		if !stepID.IsNil() {
			toolCall := &run.ToolCall{
				Entity:    cortex.NewEntity(),
				ID:        id.NewToolCallID(),
				StepID:    stepID,
				RunID:     r.ID,
				ToolName:  publicToolName(ctx, p.Name),
				Arguments: publicGenerationText(r, p.Arguments),
				Result:    publicGenerationText(r, content),
				Error:     publicToolErrorText(ctx, errText),
				// The call really was outstanding from the moment the run
				// paused until now. Stamping it as instantaneous would
				// hide exactly the latency an external tool introduces.
				StartedAt:   &startedAt,
				CompletedAt: &completedAt,
			}
			if err := e.store.CreateToolCall(ctx, toolCall); err != nil {
				e.logError("create resumed tool call", err, log.String("run_id", r.ID.String()))
			}
		}

		source := verification.ToolExternal
		if res.Execute {
			source = verification.ToolLocal
		}
		switch {
		case in.rejected && errText != "":
			outcome, source = outcomeDenied, verification.ToolLocal
			e.emitToolDenied(ctx, r.ID, p.Name, errText)
		case errText != "":
			e.emitToolFailed(ctx, r.ID, p.Name, errors.New(errText))
		default:
			e.emitToolCompleted(ctx, r.ID, p.Name, content, completedAt.Sub(startedAt))
		}
		st.recordToolResult(p.ID, outcome, source)
		st.messages = append(st.messages, llm.Message{
			Role:       "tool",
			Content:    content,
			ToolCallID: p.ID,
		})
		e.noteDone(ctx, r.ID, p.ID, content, errText, outcome)
	}
	return nil
}

// resumedCallResult returns checked-call content, error text and host outcome.
// It is also where an approved call is finally executed.
//
// Approval does not produce a result. The call was stopped before
// dispatch, so there is nothing for whoever approved it to hand back:
// the decision produces permission, and the engine still has to run the
// tool. Hence Execute, and hence the bijection rule surviving intact,
// with one result per pending call whichever kind it is.
//
// Current authorization is checked for the whole pending batch before this
// function runs. The claimed checkpoint satisfies another approval request,
// while a revoked grant prevents every pending dispatch.
func (e *Engine) resumedCallResult(ctx context.Context, s cortex.Subject, p suspension.PendingCall, res ToolResult, approved bool) (content, errText string, outcome toolOutcome) {
	if !res.Execute {
		if res.Error != "" {
			// The same payload shape an engine-side failure feeds back,
			// so the model cannot tell where the tool ran from how its
			// failure is phrased.
			return jsonResult("error", res.Error), res.Error, outcomeFailed
		}
		return res.Content, "", outcomeCompleted
	}

	if err := ctx.Err(); err != nil {
		return jsonResult("error", err.Error()), err.Error(), outcomeFailed
	}

	call := llm.ToolCall{ID: p.ID, Name: p.Name, Arguments: p.Arguments}
	content, outcome, _ = e.traceTool(ctx, call, func(ctx context.Context) (string, toolOutcome, pendingWait) {
		out, callOutcome := e.runResumedTool(ctx, s, call, approved, &errText)
		return out, callOutcome, pendingWait{}
	})
	return content, errText, outcome
}

// runResumedTool authorizes and dispatches one approved call, reporting the
// error text the run records through errText. The outcome is for the call
// tool span: an approved external call cannot run here, so it counts as a
// failure.
func (e *Engine) runResumedTool(ctx context.Context, s cortex.Subject, call llm.ToolCall, approved bool, errText *string) (string, toolOutcome) {
	if err := e.authorizeResumedTool(ctx, s, call, approved); err != nil {
		*errText = err.Error()
		return jsonResult("error", err.Error()), outcomeDenied
	}
	out, outcome, err := e.dispatchTool(ctx, s, call)
	switch outcome {
	case outcomeCompleted:
		return out, outcomeCompleted
	case outcomeUncertain:
		noteToolFailure(ctx, cortex.ErrEffectUncertain)
		*errText = cortex.ErrEffectUncertain.Error()
		return "", outcomeUncertain
	case outcomeFailed:
		noteToolFailure(ctx, err)
		*errText = err.Error()
		return out, outcomeFailed
	default:
		// An approved call that turns out to be external: the engine
		// cannot run it, and the only honest close is a failure that
		// says what to do instead. Suspending again is the alternative,
		// and it would mean a second place in the codebase where a run
		// pauses, which is the one thing this design refuses.
		msg := fmt.Sprintf("tool %q is external: an approved external call is answered by supplying its result, not by asking the engine to run it", call.Name)
		*errText = msg
		return jsonResult("error", msg), outcomeFailed
	}
}

// stepForPendingCalls finds the step whose generation asked for the
// pending calls, so a resumed call's row lands on the same step as the
// sibling calls that ran before the run paused. The continuation's
// StepIndex is the NEXT step to run, so the step that made the calls is
// the one before it.
//
// A missing step costs the audit row and nothing else: step_id is a
// non-null foreign key, and the row is best-effort in the react loop
// already (a failed CreateStep is logged, not fatal). Losing the run over
// a bookkeeping row nobody can reconstruct would be the worse trade.
func (e *Engine) stepForPendingCalls(ctx context.Context, runID id.AgentRunID, nextStep int) id.StepID {
	if nextStep <= 0 {
		return id.StepID{}
	}
	steps, err := e.store.ListSteps(ctx, runID)
	if err != nil {
		e.logError("list steps for resumed tool calls", err, log.String("run_id", runID.String()))
		return id.StepID{}
	}
	for _, s := range steps {
		if s.Index == nextStep-1 {
			return s.ID
		}
	}
	e.logWarn("no step found for resumed tool calls", nil, log.String("run_id", runID.String()))
	return id.StepID{}
}

// checkResumeAuthority is the approval gate itself, and it covers the
// whole resume rather than the Execute flag on its results.
//
// A run suspended for approval is waiting on a decision, and Resume is
// in-process API with no decision behind it. The narrow version of this
// gate refused Execute and let everything else through, which left a
// caller free to answer an escalated call with invented Content: the
// model is fed output no tool produced, the run carries on, and the
// checkpoint stays pending forever with nobody the wiser. Nothing
// privileged is dispatched that way, so it is not the hole Execute was,
// but it is the same authority arriving by a different door.
//
// So an approval suspension is resumable only through a checkpoint
// decision, which reaches this having read a pending checkpoint for the
// run, or through ContinueRun carrying one out. Every
// other pause is unaffected: an external-tool suspension never went to a
// human in the first place, and Resume is exactly how it is meant to be
// answered.
func checkResumeAuthority(reason suspension.SuspendReason, source resumeSource) error {
	switch reason {
	case suspension.ReasonApproval:
		if source == resumeSourceApproval {
			return nil
		}
		return fmt.Errorf("%w: this run is waiting on a checkpoint decision, so resolve the checkpoint rather than resuming the run", cortex.ErrRequiresApproval)

	case suspension.ReasonAgentReply:
		// The same reasoning as an approval pause, with a different
		// authority behind it. A run waiting on a peer is answered by
		// whoever holds the ledger row proving the reply is genuine, and
		// that is the bus. A caller answering it here would feed the
		// model an answer no agent gave, and the run would carry on with
		// nobody the wiser.
		if source == resumeSourceAgentReply {
			return nil
		}
		return fmt.Errorf("%w: this run is waiting on another agent's reply, which only the messaging bus can deliver", ErrNotAgentReplyResumable)

	default:
		return nil
	}
}

// validateResults enforces the bijection between pending calls and the
// results a caller returns: one result per pending id, no extras, no
// duplicates.
//
// A reconstructed conversation carrying an assistant tool call with no
// matching result is rejected by the model as malformed, and that reaches
// a user as an unexplained failure several steps later, far from the
// cause. Catching it here costs one map.
func validateResults(pending []suspension.PendingCall, results []ToolResult) error {
	byID := make(map[string]int, len(pending))
	for _, p := range pending {
		byID[p.ID] = 0
	}
	for _, r := range results {
		n, ok := byID[r.ToolCallID]
		if !ok {
			return fmt.Errorf("%w: unexpected result for call id %q", cortex.ErrResultsMismatch, r.ToolCallID)
		}
		if n > 0 {
			return fmt.Errorf("%w: duplicate result for call id %q", cortex.ErrResultsMismatch, r.ToolCallID)
		}
		byID[r.ToolCallID] = 1
	}
	for callID, n := range byID {
		if n == 0 {
			return fmt.Errorf("%w: missing result for call id %q", cortex.ErrResultsMismatch, callID)
		}
	}
	return nil
}
