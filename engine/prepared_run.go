package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/telemetry"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
)

// PreparedRunOptions contains trusted per-execution identity. It cannot replace
// the frozen prompt, model, tool allowlist or budget.
type PreparedRunOptions struct {
	SessionID     id.SessionID
	Purpose       run.ExecutionPurpose
	Provenance    run.ExecutionProvenance
	AcceptanceRef string
}

// RunPrepared executes a frozen configuration through the normal ReAct loop.
func (e *Engine) RunPrepared(ctx context.Context, p *PreparedAgent, input string, opts PreparedRunOptions) (r *run.Run, err error) {
	ctx, rt := e.startRun(ctx, modeSync, false)
	defer func() {
		if rec := recover(); rec != nil {
			rt.end(r, telemetry.ErrPanic)
			panic(rec)
		}
		rt.end(r, err)
	}()
	return e.runPrepared(ctx, p, input, opts)
}

func (e *Engine) runPrepared(ctx context.Context, p *PreparedAgent, input string, opts PreparedRunOptions) (*run.Run, error) {
	ctx, rz, err := e.startPrepared(ctx, p, input, opts)
	if err != nil {
		return nil, err
	}
	return e.continueReAct(ctx, rz.ag, rz.cfg, rz.run, rz.state, rz.startedAt)
}

// StreamPrepared streams the same prepared execution used by RunPrepared.
func (e *Engine) StreamPrepared(ctx context.Context, p *PreparedAgent, input string, opts PreparedRunOptions, events chan<- StreamEvent) (err error) {
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
	return e.streamPrepared(ctx, p, input, opts, events)
}

// streamPrepared starts the stream's goroutine, which ends the pass's run
// agent span when it finishes.
func (e *Engine) streamPrepared(ctx context.Context, p *PreparedAgent, input string, opts PreparedRunOptions, events chan<- StreamEvent) error {
	ctx, rz, err := e.startPrepared(ctx, p, input, opts)
	if err != nil {
		close(events)
		return err
	}
	rt := runTraceFrom(ctx)
	go func() {
		defer close(events)
		defer func() {
			if rec := recover(); rec != nil {
				rt.end(rz.run, telemetry.ErrPanic)
				panic(rec)
			}
			rt.end(rz.run, nil)
		}()
		if !sendEvent(ctx, events, StreamEvent{Type: EventRunStarted, Data: map[string]any{"run_id": rz.run.ID.String(), "agent_id": rz.ag.ID.String(), "prepared_digest": rz.run.Provenance.PreparedDigest}}) {
			e.cancelPublication(ctx, rz.run, 0, 0)
			return
		}
		e.continueStreamReAct(ctx, rz.ag, rz.cfg, rz.run, rz.state, rz.startedAt, events)
	}()
	return nil
}

func (e *Engine) startPrepared(ctx context.Context, p *PreparedAgent, input string, opts PreparedRunOptions) (context.Context, *resumption, error) {
	if e.store == nil {
		return nil, nil, cortex.ErrNoStore
	}
	if e.llm == nil {
		return nil, nil, cortex.ErrNoLLM
	}
	snap, err := p.snapshot()
	if err != nil {
		return nil, nil, err
	}
	scope, scopeErr := cortex.RequireScope(ctx)
	if scopeErr != nil {
		return nil, nil, scopeErr
	}
	if scope.Canonical() != snap.Scope.Canonical() {
		return nil, nil, fmt.Errorf("cortex: prepared scope mismatch: %w", cortex.ErrInvalidPrepared)
	}
	if verificationErr := e.requireVerification(snap); verificationErr != nil {
		return nil, nil, verificationErr
	}
	if snap.Config.Verification != nil && opts.AcceptanceRef != "" && opts.AcceptanceRef != snap.Config.Verification.ContractID.String() {
		return nil, nil, cortex.ErrVerificationUnavailable
	}
	sourceStatus, policyErr := e.validatePreparedPublication(ctx, snap)
	if policyErr != nil {
		return nil, nil, policyErr
	}
	if revisionErr := e.checkPreparedRevisions(snap); revisionErr != nil {
		return nil, nil, revisionErr
	}
	provenance := snap.Provenance
	provenance.PublicationStatus = mergePublicationStatus(provenance.PublicationStatus, sourceStatus)
	provenance.PreparedDigest = p.Digest()
	provenance.Purpose = opts.Purpose
	if provenance.Purpose == "" {
		provenance.Purpose = run.PurposeNormal
	}
	switch provenance.Purpose {
	case run.PurposeNormal, run.PurposeEvaluation, run.PurposeOptimizationTrial, run.PurposeOptimizationController:
	default:
		return nil, nil, fmt.Errorf("cortex: unknown execution purpose: %w", cortex.ErrInvalidPrepared)
	}
	if opts.Provenance.AssignmentDigest != "" {
		parent, parseErr := id.Parse(opts.Provenance.ParentRunID)
		if !evidence.ValidDigest(opts.Provenance.AssignmentDigest) || parseErr != nil || parent.IsNil() || parent.Prefix() != id.PrefixAgentRun {
			return nil, nil, cortex.ErrInvalidPrepared
		}
	}
	provenance.AssignmentDigest = opts.Provenance.AssignmentDigest
	provenance.ParentRunID = opts.Provenance.ParentRunID
	provenance.ExperimentID = opts.Provenance.ExperimentID
	provenance.CandidateID = opts.Provenance.CandidateID
	provenance.EnvironmentReceiptID = opts.Provenance.EnvironmentReceiptID
	provenance.TrialID = opts.Provenance.TrialID
	provenance.HarnessID = opts.Provenance.HarnessID
	provenance.HarnessVersion = opts.Provenance.HarnessVersion
	provenance.AcceptanceRef = opts.AcceptanceRef
	ctx = withExecutionProvenance(ctx, provenance)
	cfg := snap.Config
	if policyErr := e.requirePublication(cfg.Publication); policyErr != nil {
		return nil, nil, policyErr
	}
	if deadline, ok := ctx.Deadline(); ok && (cfg.Budget.Deadline.IsZero() || deadline.Before(cfg.Budget.Deadline)) {
		cfg.Budget.Deadline = deadline
	}
	sessionID, err := e.resolveSession(ctx, snap.Agent.ID, opts.SessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("cortex: prepared session: %w", err)
	}
	now := time.Now().UTC()
	r := &run.Run{Entity: cortex.NewEntity(), ID: id.NewAgentRunID(), AgentID: snap.Agent.ID, SessionID: sessionID, Scope: scope, State: run.StateRunning, Input: input, StartedAt: &now, PersonaRef: cfg.PersonaRef, Provenance: provenance, Budget: cfg.Budget}
	safeInput, scanErr := e.checkPublication(ctx, &snap.Agent, r, cfg.Publication, input, "user_input", safety.DirectionInput)
	if scanErr != nil {
		return nil, nil, scanErr
	}
	input = string(safeInput)
	r.Input = input
	var messages []llm.Message
	if cfg.Verification != nil {
		messages, err = e.loadPreparedHistory(ctx, snap, r)
		if err != nil {
			return nil, nil, err
		}
	}
	intent, intentErr := e.buildRequiredIntent(ctx, p, cfg, r, messages)
	if intentErr != nil {
		return nil, nil, intentErr
	}
	ctx = withExecutionProvenance(ctx, r.Provenance)
	if err = e.store.CreateRun(ctx, r); err != nil {
		return nil, nil, fmt.Errorf("cortex: create prepared run: %w", err)
	}
	if cfg.Verification != nil {
		storage, captureOK := e.store.(requiredVerificationStore)
		if !captureOK || r.Revision != 1 {
			return nil, nil, cortex.ErrVerificationUnavailable
		}
		if registerErr := storage.CreateCaptureIntent(ctx, intent); registerErr != nil {
			e.failRun(ctx, r, r.AgentID, cortex.ErrVerificationUnavailable, now)
			return nil, nil, cortex.ErrVerificationUnavailable
		}
	}
	runTraceFrom(ctx).bind(r, cfg.Model)
	e.extensions.EmitRunStarted(ctx, r.AgentID, r.ID, input)
	if cfg.Verification == nil {
		messages, err = e.loadPreparedHistory(ctx, snap, r)
		if err != nil {
			return nil, nil, e.stopPublication(ctx, r, &reactState{}, err)
		}
	}
	st := &reactState{messages: messages, systemPrompt: prompt.Assemble(snap.Sections), sessionID: sessionID, newMessagesFrom: len(messages), prepared: &PreparedAgent{payload: append([]byte(nil), p.payload...), digest: p.digest}, preparedSnapshot: snap, provenance: r.Provenance}
	st.messages = append(st.messages, llm.Message{Role: "user", Content: input})
	return ctx, &resumption{ag: &snap.Agent, cfg: cfg, run: r, state: st, startedAt: now}, nil
}

func (e *Engine) checkPreparedRevisions(snap *preparedSnapshot) error {
	if pinned := snap.Provenance.ModelRevision; pinned != "" && e.modelRevisions[snap.Config.Model] != pinned {
		return fmt.Errorf("cortex: model revision changed: %w", cortex.ErrInvalidPrepared)
	}
	for _, tool := range snap.Provenance.Tools {
		if tool.Revision != "" && e.toolRevisions[tool.Name] != tool.Revision {
			return fmt.Errorf("cortex: tool %s revision changed: %w", tool.Name, cortex.ErrInvalidPrepared)
		}
	}
	return nil
}

func preparedOptions(overrides *RunOverrides) PreparedRunOptions {
	var opts PreparedRunOptions
	if overrides != nil {
		opts.SessionID = overrides.SessionID
	}
	return opts
}

func (e *Engine) loadPreparedHistory(ctx context.Context, snap *preparedSnapshot, r *run.Run) ([]llm.Message, error) {
	history, err := e.store.LoadConversation(ctx, r.AgentID, r.SessionID, 100)
	if err != nil {
		if snap.Config.Verification != nil {
			return nil, cortex.ErrVerificationUnavailable
		}
		r.Provenance.Diagnostics = append(r.Provenance.Diagnostics, "conversation history unavailable")
	}
	messages := memoryToLLM(history)
	if scanErr := e.checkMessages(ctx, &snap.Agent, r, snap.Config.Publication, messages); scanErr != nil {
		return nil, scanErr
	}
	return messages, nil
}
