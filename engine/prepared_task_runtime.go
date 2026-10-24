package engine

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/internal/telemetry"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
)

// WithTaskRunAdmitter installs guarded, atomic child creation for prepared tasks.
// Use coordination.NewTaskRunAdmitter with the same durable store as the engine.
// A confirmed fresh receipt is required to begin execution. Historical or
// ambiguous creation must be reconciled, never retried as provider invocation.
func WithTaskRunAdmitter(admitter coordination.TaskRunAdmitter) Option {
	return func(e *Engine) error {
		if hostport.IsNil(admitter) {
			return coordination.ErrInvalidClaim
		}
		e.taskRunAdmitter = admitter
		return nil
	}
}

// RunPreparedTask admits and executes the exact request from PrepareTaskRun.
// It loads authorized captured inputs, never current conversation history. Both
// a TaskRunAdmitter and TaskCallGuard are required. An ambiguous or historical
// start returns ErrTaskRunReconciliationRequired without invoking a provider.
func (e *Engine) RunPreparedTask(ctx context.Context, p *PreparedAgent, request coordination.TaskRunRequest) (r *run.Run, err error) {
	return e.runPreparedTask(ctx, p, request, nil)
}

func (e *Engine) runPreparedTask(ctx context.Context, p *PreparedAgent, request coordination.TaskRunRequest, started func() error) (r *run.Run, err error) {
	ctx, rt := e.startRun(ctx, modeSync, false)
	defer func() {
		if rec := recover(); rec != nil {
			rt.end(r, telemetry.ErrPanic)
			panic(rec)
		}
		rt.end(r, err)
	}()
	ctx, rz, err := e.startPreparedTask(ctx, p, request)
	if err != nil {
		return nil, err
	}
	if started != nil {
		if activationErr := started(); activationErr != nil {
			return rz.run, activationErr
		}
	}
	return e.continueReAct(ctx, rz.ag, rz.cfg, rz.run, rz.state, rz.startedAt)
}

// StreamPreparedTask streams the same admitted execution as RunPreparedTask.
// It closes events on admission failure and owns the channel after success.
func (e *Engine) StreamPreparedTask(ctx context.Context, p *PreparedAgent, request coordination.TaskRunRequest, events chan<- StreamEvent) (err error) {
	ctx, rt := e.startRun(ctx, modeStream, false)
	defer func() {
		if rec := recover(); rec != nil {
			rt.end(nil, telemetry.ErrPanic)
			panic(rec)
		}
		if err != nil {
			rt.end(nil, err)
		}
	}()
	ctx, rz, err := e.startPreparedTask(ctx, p, request)
	if err != nil {
		close(events)
		return err
	}
	e.launchPreparedStream(ctx, rz, events)
	return nil
}

func (e *Engine) startPreparedTask(ctx context.Context, p *PreparedAgent, request coordination.TaskRunRequest) (context.Context, *resumption, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if e.store == nil {
		return nil, nil, cortex.ErrNoStore
	}
	if e.llm == nil {
		return nil, nil, cortex.ErrNoLLM
	}
	if hostport.IsNil(e.taskRunAdmitter) || hostport.IsNil(e.taskCallGuard) {
		return nil, nil, cortex.ErrTaskCallUnavailable
	}
	req, _, err := request.Freeze()
	if err != nil {
		return nil, nil, err
	}
	snap, err := p.snapshot()
	if err != nil {
		return nil, nil, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, nil, err
	}
	if snap.Scope.Canonical() != scope.Canonical() || req.Run.Scope.Canonical() != scope.Canonical() || req.Run.AgentID != snap.Agent.ID || req.Run.PersonaRef != snap.Config.PersonaRef || req.Run.Provenance.TaskClaimID != req.Claim.ClaimID.String() || snap.Config.Verification == nil {
		return nil, nil, cortex.ErrInvalidPrepared
	}
	if verificationErr := e.requireVerification(snap); verificationErr != nil {
		return nil, nil, verificationErr
	}
	if revisionErr := e.checkPreparedRevisions(snap); revisionErr != nil {
		return nil, nil, revisionErr
	}
	status, err := e.validatePreparedPublication(ctx, snap)
	if err != nil {
		return nil, nil, err
	}
	cfg := snap.Config
	if req.Run.Budget.Deadline.After(cfg.Budget.Deadline) {
		return nil, nil, cortex.ErrInvalidPrepared
	}
	cfg.Budget.Deadline = req.Run.Budget.Deadline
	if !cfg.Budget.Equal(req.Run.Budget) || !time.Now().Before(cfg.Budget.Deadline) {
		return nil, nil, cortex.ErrInvalidPrepared
	}
	if publicationErr := e.requirePublication(cfg.Publication); publicationErr != nil {
		return nil, nil, publicationErr
	}
	if !requiredIntentMatchesPolicy(req.Intent, cfg.Verification, &req.Run) || !req.Intent.Template.Deadline.After(e.verificationWorker.config.Clock.Now()) || req.Intent.MaxTranscriptBytes > 1<<20 || req.Intent.MaxCandidateBytes > 1<<20 || req.Intent.MaxCandidateBytes > int64(cfg.Publication.MaxOutputBytes) {
		return nil, nil, cortex.ErrVerificationUnavailable
	}
	if provenanceErr := validatePreparedTaskProvenance(p, snap, req, status); provenanceErr != nil {
		return nil, nil, provenanceErr
	}
	ctx = withExecutionProvenance(ctx, req.Run.Provenance)
	inputs, err := e.loadRequiredInputs(ctx, &req.Run, req.Intent)
	if err != nil {
		return nil, nil, err
	}
	if inputs.Model != snap.Config.Model {
		return nil, nil, cortex.ErrVerificationUnavailable
	}
	if candidateErr := e.authorizeCandidate(ctx, req.Intent.Template.Binding.Candidate); candidateErr != nil {
		return nil, nil, candidateErr
	}
	if transcriptErr := e.authorizeCandidate(ctx, *req.Intent.Transcript); transcriptErr != nil {
		return nil, nil, transcriptErr
	}
	offered, _, err := req.Freeze()
	if err != nil {
		return nil, nil, err
	}
	start, err := e.taskRunAdmitter.Admit(ctx, offered)
	if err != nil {
		return nil, nil, cortex.ErrTaskRunReconciliationRequired
	}
	if !start.Fresh || start.Validate(req) != nil {
		return nil, nil, cortex.ErrTaskRunReconciliationRequired
	}
	if err = ctx.Err(); err != nil {
		return nil, nil, cortex.ErrTaskRunReconciliationRequired
	}
	child, err := req.InitialRun(start.CreatedAt)
	if err != nil {
		return nil, nil, cortex.ErrTaskRunReconciliationRequired
	}
	stored, err := e.store.GetRun(ctx, child.ID)
	if err != nil || stored == nil {
		return nil, nil, cortex.ErrTaskRunReconciliationRequired
	}
	// SQL drivers may return local-zone timestamps for an identical UTC instant.
	storedCopy := *stored
	storedCopy.CreatedAt, storedCopy.UpdatedAt = stored.CreatedAt.UTC(), stored.UpdatedAt.UTC()
	if stored.StartedAt != nil {
		at := stored.StartedAt.UTC()
		storedCopy.StartedAt = &at
	}
	data, marshalErr := json.Marshal(storedCopy)
	if marshalErr != nil || evidence.Digest(data) != start.RunDigest {
		return nil, nil, cortex.ErrTaskRunReconciliationRequired
	}
	storage, ok := e.store.(requiredVerificationStore)
	if !ok {
		return nil, nil, cortex.ErrTaskRunReconciliationRequired
	}
	intent, err := storage.GetRunCaptureIntent(ctx, child.ID)
	if err != nil || intent.Validate() != nil || intent.Digest != req.Intent.Digest {
		return nil, nil, cortex.ErrTaskRunReconciliationRequired
	}
	runTraceFrom(ctx).bind(&child, cfg.Model)
	e.extensions.EmitRunStarted(ctx, child.AgentID, child.ID, child.Input)
	st := &reactState{messages: inputs.History, systemPrompt: prompt.Assemble(snap.Sections), sessionID: child.SessionID, newMessagesFrom: len(inputs.History), prepared: &PreparedAgent{payload: append([]byte(nil), p.payload...), digest: p.digest}, preparedSnapshot: snap, provenance: child.Provenance}
	st.messages = append(st.messages, llm.Message{Role: "user", Content: inputs.Input})
	return ctx, &resumption{ag: &snap.Agent, cfg: cfg, run: &child, state: st, startedAt: start.CreatedAt}, nil
}

func validatePreparedTaskProvenance(p *PreparedAgent, snap *preparedSnapshot, req coordination.TaskRunRequest, status safety.PublicationStatus) error {
	actual := req.Run.Provenance
	switch actual.PublicationStatus {
	case safety.StatusApproved, safety.StatusFlagged, safety.StatusRedacted, safety.StatusUnscanned:
	default:
		return cortex.ErrInvalidPrepared
	}
	if mergePublicationStatus(actual.PublicationStatus, status) != actual.PublicationStatus {
		return cortex.ErrInvalidPrepared
	}
	expected, err := preparedRunProvenance(p, snap, PreparedRunOptions{Purpose: run.PurposeNormal, Provenance: run.ExecutionProvenance{AssignmentDigest: actual.AssignmentDigest, ParentRunID: actual.ParentRunID}}, status)
	if err != nil {
		return err
	}
	expected.PublicationStatus = actual.PublicationStatus
	expected.TaskClaimID = req.Claim.ClaimID.String()
	expected.ExecutionInputsDigest = req.Intent.Template.Binding.Context.ExecutionInputs.SHA256
	expected.VerificationRequired = true
	expected.VerificationIntentID = req.Intent.ID.String()
	expected.AcceptanceRef = snap.Config.Verification.ContractID.String()
	if !reflect.DeepEqual(expected, actual) {
		return cortex.ErrInvalidPrepared
	}
	return nil
}
