package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/run"
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
func (e *Engine) RunPrepared(ctx context.Context, p *PreparedAgent, input string, opts PreparedRunOptions) (*run.Run, error) {
	ctx, rz, err := e.startPrepared(ctx, p, input, opts)
	if err != nil {
		return nil, err
	}
	return e.continueReAct(ctx, rz.ag, rz.cfg, rz.run, rz.state, input, rz.startedAt)
}

// StreamPrepared streams the same prepared execution used by RunPrepared.
func (e *Engine) StreamPrepared(ctx context.Context, p *PreparedAgent, input string, opts PreparedRunOptions, events chan<- StreamEvent) error {
	ctx, rz, err := e.startPrepared(ctx, p, input, opts)
	if err != nil {
		close(events)
		return err
	}
	go func() {
		defer close(events)
		events <- StreamEvent{Type: EventRunStarted, Data: map[string]any{"run_id": rz.run.ID.String(), "agent_id": rz.ag.ID.String(), "prepared_digest": rz.run.Provenance.PreparedDigest}}
		e.continueStreamReAct(ctx, rz.ag, rz.cfg, rz.run, rz.state, input, rz.startedAt, events)
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
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, nil, cortex.ErrNoScope
	}
	if scope.Canonical() != snap.Scope.Canonical() {
		return nil, nil, fmt.Errorf("cortex: prepared scope mismatch: %w", cortex.ErrInvalidPrepared)
	}
	if revisionErr := e.checkPreparedRevisions(snap); revisionErr != nil {
		return nil, nil, revisionErr
	}
	provenance := snap.Provenance
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
	if deadline, ok := ctx.Deadline(); ok && (cfg.Budget.Deadline.IsZero() || deadline.Before(cfg.Budget.Deadline)) {
		cfg.Budget.Deadline = deadline
	}
	sessionID, err := e.resolveSession(ctx, snap.Agent.ID, opts.SessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("cortex: prepared session: %w", err)
	}
	now := time.Now().UTC()
	r := &run.Run{Entity: cortex.NewEntity(), ID: id.NewAgentRunID(), AgentID: snap.Agent.ID, SessionID: sessionID, Scope: scope, State: run.StateRunning, Input: input, StartedAt: &now, PersonaRef: cfg.PersonaRef, Provenance: provenance, Budget: cfg.Budget}
	if err = e.store.CreateRun(ctx, r); err != nil {
		return nil, nil, fmt.Errorf("cortex: create prepared run: %w", err)
	}
	e.extensions.EmitRunStarted(ctx, r.AgentID, r.ID, input)
	history, err := e.store.LoadConversation(ctx, r.AgentID, sessionID, 100)
	if err != nil {
		// Preserve legacy best-effort history while recording its absence.
		r.Provenance.Diagnostics = append(r.Provenance.Diagnostics, "conversation history unavailable")
	}
	messages := memoryToLLM(history)
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
