package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
)

type preparedTeamContinuation struct {
	SchemaVersion int                    `json:"schema_version"`
	Prepared      *PreparedOrchestration `json:"prepared"`
	Options       PreparedRunOptions     `json:"options"`
}

func preparedTeamPayload(p *PreparedOrchestration, opts PreparedRunOptions) ([]byte, error) {
	return json.Marshal(preparedTeamContinuation{SchemaVersion: 1, Prepared: p, Options: opts})
}

// ResumePreparedOrchestration continues a waiting team or claims an expired
// coordinator from its original snapshot. Unleased running parents are refused.
// Resume paused children through their existing authorized resume APIs first.
// This call never restarts a child with a retained invocation identity.
func (e *Engine) ResumePreparedOrchestration(ctx context.Context, team id.OrchestrationID) (rec *orchestration.Run, err error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	if e.llm == nil {
		return nil, cortex.ErrNoLLM
	}
	runner, err := e.restorePreparedTeam(ctx, team)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithDeadline(ctx, runner.policy.Deadline)
	defer cancel()
	return orchestration.NewCoordinatedService(runner, runner, e.store, registryHookEmitter{eng: e}).ResumeContinuation(ctx, team)
}

func (e *Engine) restorePreparedTeam(ctx context.Context, team id.OrchestrationID) (*preparedTeamRunner, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	storage, ok := e.store.(orchestration.ContinuationStore)
	if !ok {
		return nil, cortex.ErrOrchestrationContinuationUnavailable
	}
	c, err := storage.GetOrchestrationContinuation(ctx, team)
	if err != nil {
		return nil, err
	}
	if c.OrchestrationID != team || c.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return nil, cortex.ErrInvalidContinuation
	}
	var saved preparedTeamContinuation
	if json.Unmarshal(c.Payload, &saved) != nil || saved.SchemaVersion != 1 || saved.Prepared == nil {
		return nil, cortex.ErrInvalidContinuation
	}
	budget, ok := e.store.(orchestration.BudgetStore)
	if !ok {
		return nil, cortex.ErrBudgetUnavailable
	}
	account, err := budget.GetOrchestrationBudgetSnapshot(ctx, team)
	if err != nil {
		return nil, err
	}
	if account.Closed || account.Overflow || !account.Policy.Limits.Covers(account.Charged) {
		return nil, cortex.ErrInvalidPrepared
	}
	saved.Options.OrchestrationBudget = &account.Policy
	view, policy, err := e.preparedTeamPolicy(ctx, saved.Prepared, saved.Options)
	if err != nil {
		return nil, err
	}
	// The persisted host snapshot and strategy must describe exactly the same work.
	encoded, encodeErr := json.Marshal(view.Config)
	recorded, recordErr := json.Marshal(c.Config)
	if encodeErr != nil || recordErr != nil {
		return nil, cortex.ErrInvalidContinuation
	}
	if !bytes.Equal(encoded, recorded) || policy.ExecutionID != team {
		return nil, cortex.ErrInvalidContinuation
	}
	saved.Options.OrchestrationBudget = nil
	return &preparedTeamRunner{eng: e, policy: policy, view: view, opts: saved.Options}, nil
}

func (r *preparedTeamRunner) ObserveOrchestrationChild(ctx context.Context, in orchestration.InvocationRecord) (*orchestration.AgentResult, error) {
	if in.Intent.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return nil, cortex.ErrInvalidPrepared
	}
	child, err := r.observePreparedChild(ctx, in.Intent.AgentName, in.RunID)
	if err != nil {
		return nil, err
	}
	if child.State == run.StateRunning || childWaiting(child, false) {
		return &orchestration.AgentResult{AgentName: in.Intent.AgentName, RunID: child.ID, Err: cortex.ErrOrchestrationWaiting}, cortex.ErrOrchestrationWaiting
	}
	return orchestrationAgentResult(in.Intent.AgentName, child, nil)
}

func (r *preparedTeamRunner) observePreparedChild(ctx context.Context, name string, key id.AgentRunID) (*run.Run, error) {
	child, err := r.eng.store.GetRun(ctx, key)
	if err != nil {
		return nil, err
	}
	if child == nil || child.ID != key {
		return nil, cortex.ErrInvalidPrepared
	}
	captured := r.view.Participants[name]
	if captured == nil {
		return nil, cortex.ErrInvalidPrepared
	}
	snap, err := captured.snapshot()
	if err != nil {
		return nil, err
	}
	if child.AgentID != snap.Agent.ID || child.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() || child.Provenance.PreparedDigest != captured.Digest() || child.Provenance.OrchestrationID != r.policy.ExecutionID.String() || child.Provenance.OrchestrationDigest != r.policy.PreparedDigest {
		return nil, cortex.ErrInvalidPrepared
	}
	team, err := r.eng.store.GetOrchestrationRun(ctx, r.policy.ExecutionID)
	if err != nil || team == nil || !slices.Contains(team.AgentRunIDs, child.ID) {
		return nil, errors.Join(err, cortex.ErrInvalidPrepared)
	}
	if bindingErr := r.eng.checkOrchestrationObservation(ctx, child, snap); bindingErr != nil {
		return nil, bindingErr
	}
	allocation, err := preparedTeamAllocation(snap)
	if err != nil {
		return nil, err
	}
	if usageErr := r.checkUsage(child, allocation); usageErr != nil {
		return nil, usageErr
	}
	return r.eng.recoveredTeamResult(ctx, child, snap)
}
