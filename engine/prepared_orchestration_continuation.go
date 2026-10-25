package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

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

// ResumePreparedOrchestration continues a waiting team from its original snapshot.
// Resume paused children through their existing authorized resume APIs first.
// This call never restarts a child with a retained invocation identity.
func (e *Engine) ResumePreparedOrchestration(ctx context.Context, team id.OrchestrationID) (rec *orchestration.Run, err error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	if e.llm == nil {
		return nil, cortex.ErrNoLLM
	}
	storage, ok := e.store.(orchestration.ContinuationStore)
	if !ok {
		return nil, cortex.ErrOrchestrationContinuationUnavailable
	}
	c, err := storage.GetOrchestrationContinuation(ctx, team)
	if err != nil {
		return nil, err
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
	ctx, cancel := context.WithDeadline(ctx, policy.Deadline)
	defer cancel()
	saved.Options.OrchestrationBudget = nil
	runner := &preparedTeamRunner{eng: e, budget: budget, policy: policy, view: view, opts: saved.Options}
	defer func() {
		if rec != nil && rec.Status != orchestration.StatusWaiting {
			writeCtx, finish := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			err = errors.Join(err, budget.CloseOrchestrationBudget(writeCtx, team))
			finish()
		}
	}()
	return orchestration.NewContinuableService(runner, runner, e.store, registryHookEmitter{eng: e}).ResumeContinuation(ctx, team)
}

func (r *preparedTeamRunner) ObserveOrchestrationChild(ctx context.Context, in orchestration.InvocationRecord) (*orchestration.AgentResult, error) {
	child, err := r.eng.store.GetRun(ctx, in.RunID)
	if err != nil {
		return nil, err
	}
	captured := r.view.Participants[in.Intent.AgentName]
	if captured == nil {
		return nil, cortex.ErrInvalidPrepared
	}
	snap, err := captured.snapshot()
	if err != nil {
		return nil, err
	}
	if child.AgentID != snap.Agent.ID || child.Scope.Canonical() != in.Intent.Scope.Canonical() || child.Provenance.PreparedDigest != captured.Digest() || child.Provenance.OrchestrationID != r.policy.ExecutionID.String() || child.Provenance.OrchestrationDigest != r.policy.PreparedDigest {
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
	if usageErr := r.checkUsage(ctx, child, allocation); usageErr != nil {
		return nil, usageErr
	}
	if child.State == run.StatePaused || child.State == run.StateRunning {
		return &orchestration.AgentResult{AgentName: in.Intent.AgentName, RunID: child.ID, Err: cortex.ErrOrchestrationWaiting}, cortex.ErrOrchestrationWaiting
	}
	return orchestrationAgentResult(in.Intent.AgentName, child, nil)
}
