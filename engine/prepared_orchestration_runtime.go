package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/telemetry"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/session"
)

// RunPreparedOrchestration executes frozen participants under explicit shared
// capacity. Each child gets a fresh nondefault session. Token-only accounts are
// supported; billing settlement remains host-owned. Messaging tools require an
// isolated route and are refused until that route is available.
func (e *Engine) RunPreparedOrchestration(ctx context.Context, p *PreparedOrchestration, input string, opts PreparedRunOptions) (rec *orchestration.Run, err error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	if e.llm == nil {
		return nil, cortex.ErrNoLLM
	}
	budget, ok := e.store.(orchestration.BudgetStore)
	if !ok {
		return nil, cortex.ErrBudgetUnavailable
	}
	view, policy, err := e.preparedTeamPolicy(ctx, p, opts)
	if err != nil {
		return nil, err
	}
	if createErr := budget.CreateOrchestrationBudget(ctx, policy); createErr != nil {
		return nil, createErr
	}
	opts.OrchestrationBudget = nil
	ctx, cancel := context.WithDeadline(ctx, policy.Deadline)
	defer cancel()
	ctx, span := telemetry.Tracer().Start(ctx, spanRunOrchestration)
	hooks := &preparedTeamHooks{registryHookEmitter: registryHookEmitter{eng: e}}
	defer func() {
		recovered := recover()
		if hooks.started {
			writeCtx, finish := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			err = errors.Join(err, budget.CloseOrchestrationBudget(writeCtx, policy.ExecutionID))
			finish()
		}
		if recovered != nil {
			e.finishOrchestration(span, rec, telemetry.ErrPanic)
			panic(recovered)
		}
		e.finishOrchestration(span, rec, err)
	}()
	runner := &preparedTeamRunner{eng: e, budget: budget, policy: policy, view: view, opts: opts}
	return orchestration.NewReservedService(runner, e.store, hooks).RunConfigWithID(ctx, view.Config, input, policy.ExecutionID)
}

type preparedTeamHooks struct {
	registryHookEmitter
	started bool
}

func (h *preparedTeamHooks) OrchestrationStarted(ctx context.Context, key id.OrchestrationID, strategy string) {
	h.started = true
	h.registryHookEmitter.OrchestrationStarted(ctx, key, strategy)
}

func (e *Engine) preparedTeamPolicy(ctx context.Context, p *PreparedOrchestration, opts PreparedRunOptions) (PreparedOrchestrationView, orchestration.BudgetPolicy, error) {
	var empty PreparedOrchestrationView
	var noPolicy orchestration.BudgetPolicy
	if opts.OrchestrationBudget == nil || !opts.SessionID.IsNil() || hasOrchestrationBinding(opts.Provenance) {
		return empty, noPolicy, cortex.ErrInvalidPrepared
	}
	view, err := p.Inspect()
	if err != nil {
		return empty, noPolicy, err
	}
	policy, _, err := opts.OrchestrationBudget.Freeze()
	if err != nil {
		return empty, noPolicy, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return empty, noPolicy, err
	}
	if !policy.TokenOnly || policy.PreparedDigest != p.Digest() || policy.Scope.Canonical() != scope.Canonical() || view.Config.Scope.Canonical() != scope.Canonical() || len(view.Participants) != len(policy.Participants) || !time.Now().Before(policy.Deadline) {
		return empty, noPolicy, cortex.ErrInvalidPrepared
	}
	if policy.ParticipantDigests == nil {
		policy.ParticipantDigests = make(map[string]string, len(view.Participants))
	}
	for name, child := range view.Participants {
		snap, readErr := child.snapshot()
		if readErr != nil {
			return empty, noPolicy, readErr
		}
		allocation, boundErr := preparedTeamAllocation(snap)
		if boundErr != nil || !policy.Participants[name].Covers(allocation) {
			return empty, noPolicy, cortex.ErrInvalidPrepared
		}
		if pinned := policy.ParticipantDigests[name]; pinned != "" && pinned != child.Digest() {
			return empty, noPolicy, cortex.ErrInvalidPrepared
		}
		policy.ParticipantDigests[name] = child.Digest()
		for _, tool := range snap.Tools {
			if tool.Name == toolAgentSend || tool.Name == toolAgentAsk || tool.Name == toolAgentInbox {
				return empty, noPolicy, cortex.ErrInvalidPrepared
			}
		}
		if _, provenanceErr := preparedRunProvenance(child, snap, opts, snap.Provenance.PublicationStatus); provenanceErr != nil {
			return empty, noPolicy, provenanceErr
		}
		if revisionErr := e.checkPreparedRevisions(snap); revisionErr != nil {
			return empty, noPolicy, revisionErr
		}
		if verificationErr := e.requireVerification(snap); verificationErr != nil {
			return empty, noPolicy, verificationErr
		}
	}
	policy, _, err = policy.Freeze()
	return view, policy, err
}

func preparedTeamAllocation(snap *preparedSnapshot) (execution.Resources, error) {
	b := snap.Config.Budget
	if b.Validate() != nil || b.MaxSteps < 1 || b.MaxTokens < 1 || b.Deadline.IsZero() || (len(snap.Tools) > 0 && b.MaxToolCalls < 1) {
		return execution.Resources{}, cortex.ErrInvalidPrepared
	}
	tokens := b.MaxTokens
	if snap.Config.Verification != nil {
		tokens = snap.Config.Verification.TotalTokens
	}
	return execution.Resources{Steps: int64(b.MaxSteps), Tokens: int64(tokens), ToolCalls: int64(b.MaxToolCalls), FanOut: 1}, nil
}

type preparedTeamRunner struct {
	eng    *Engine
	budget orchestration.BudgetStore
	policy orchestration.BudgetPolicy
	view   PreparedOrchestrationView
	opts   PreparedRunOptions
	closed atomic.Bool
}

func (r *preparedTeamRunner) ReserveAgent(ctx context.Context, team id.OrchestrationID, name, input string, override *orchestration.RunOpts) (*orchestration.ReservedInvocation, error) {
	if r.closed.Load() {
		return nil, execution.ErrBudgetExceeded
	}
	child := r.view.Participants[name]
	if child == nil || team != r.policy.ExecutionID {
		return nil, cortex.ErrInvalidPrepared
	}
	snap, err := child.snapshot()
	if err != nil {
		return nil, err
	}
	if override != nil && ((override.Model != "" && override.Model != snap.Config.Model) || override.Temperature != nil || override.MaxSteps != 0 || override.SystemPrompt != "") {
		return nil, cortex.ErrInvalidPrepared
	}
	allocation, err := preparedTeamAllocation(snap)
	if err != nil {
		return nil, err
	}
	deadline := snap.Config.Budget.Deadline
	if r.policy.Deadline.Before(deadline) {
		deadline = r.policy.Deadline
	}
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	key := id.NewAgentRunID()
	request := execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: team, PlanDigest: r.policy.PreparedDigest, TaskID: key, Scope: r.policy.Scope, OwnerRef: name, Kind: execution.ReservationLeaf, Allocation: allocation, Deadline: deadline}
	inv := &orchestration.ReservedInvocation{RunID: key}
	if _, err = r.budget.ReserveOrchestrationChild(ctx, request); err != nil {
		if errors.Is(err, execution.ErrBudgetExceeded) || errors.Is(err, execution.ErrInvalidReservation) {
			return nil, err
		}
		return inv, err
	}
	opts := r.opts
	opts.SessionID = id.NewSessionID()
	opts.Provenance.OrchestrationID = team.String()
	opts.Provenance.OrchestrationDigest = r.policy.PreparedDigest
	opts.Provenance.OrchestrationReservationID = request.ID.String()
	if sessionErr := r.eng.store.CreateSession(ctx, &session.Session{ID: opts.SessionID, AgentID: snap.Agent.ID, Title: "Orchestration"}); sessionErr != nil {
		return inv, sessionErr
	}
	prepareCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	_, draft, err := r.eng.prepareRun(prepareCtx, child, input, opts, key)
	if err != nil {
		return inv, err
	}
	inv.Execute = func(execCtx context.Context) (*orchestration.AgentResult, error) {
		if r.closed.Load() {
			return nil, execution.ErrBudgetExceeded
		}
		record, runErr := r.execute(execCtx, draft)
		if record != nil && (int64(record.Usage.Tokens) > allocation.Tokens || int64(record.Usage.Steps) > allocation.Steps || int64(record.Usage.ToolCalls) > allocation.ToolCalls) {
			r.closed.Store(true)
			closeCtx, finish := context.WithTimeout(context.WithoutCancel(execCtx), 5*time.Second)
			runErr = errors.Join(runErr, execution.ErrBudgetExceeded, r.budget.CloseOrchestrationBudget(closeCtx, team))
			finish()
		}
		return orchestrationAgentResult(name, record, runErr)
	}
	return inv, nil
}

func (r *preparedTeamRunner) execute(ctx context.Context, draft *preparedRunDraft) (record *run.Run, err error) {
	ctx, cancel := context.WithDeadline(ctx, draft.cfg.Budget.Deadline)
	defer cancel()
	ctx = withExecutionProvenance(ctx, draft.run.Provenance)
	ctx, rt := r.eng.startRun(ctx, modeSync, false)
	defer func() {
		if recovered := recover(); recovered != nil {
			rt.end(record, telemetry.ErrPanic)
			panic(recovered)
		}
		rt.end(record, err)
	}()
	ctx, rz, err := r.eng.activatePrepared(ctx, draft)
	if err != nil {
		if rz != nil {
			return rz.run, err
		}
		return nil, err
	}
	return r.eng.continueReAct(ctx, rz.ag, rz.cfg, rz.run, rz.state, rz.startedAt)
}
