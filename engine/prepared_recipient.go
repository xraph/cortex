package engine

import (
	"context"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

// ObservePreparedRecipient reads an existing child's outcome from the original
// prepared team. The host must authorize this exact team and run before private
// reads and before returning output. No child is reserved, executed or resumed.
// Only the run's published output is returned; result verification is unchanged.
func (e *Engine) ObservePreparedRecipient(ctx context.Context, req a2a.RecipientRequest, authorize func(context.Context, id.OrchestrationID, id.AgentRunID) error) (a2a.RecipientObservation, error) {
	if authorize == nil || req.OrchestrationID.Prefix() != id.PrefixOrchestration || req.RunID.Prefix() != id.PrefixAgentRun || req.AgentName == "" || len(req.AgentName) > 256 || !utf8.ValidString(req.AgentName) {
		return a2a.RecipientObservation{}, cortex.ErrInvalidPrepared
	}
	if _, err := cortex.RequireScope(ctx); err != nil {
		return a2a.RecipientObservation{}, err
	}
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return authorize(ctx, req.OrchestrationID, req.RunID)
	}
	if err := check(); err != nil {
		return a2a.RecipientObservation{}, err
	}
	runner, err := e.restorePreparedTeam(ctx, req.OrchestrationID)
	if err != nil {
		return a2a.RecipientObservation{}, err
	}
	result, err := runner.ObserveRecipient(ctx, req)
	if err != nil {
		return a2a.RecipientObservation{}, err
	}
	if authErr := check(); authErr != nil {
		return a2a.RecipientObservation{}, authErr
	}
	return result, nil
}

func (r *preparedTeamRunner) ObserveRecipient(ctx context.Context, req a2a.RecipientRequest) (a2a.RecipientObservation, error) {
	if req.OrchestrationID != r.policy.ExecutionID {
		return a2a.RecipientObservation{}, cortex.ErrInvalidPrepared
	}
	child, err := r.observePreparedChild(ctx, req.AgentName, req.RunID)
	if err != nil {
		return a2a.RecipientObservation{}, err
	}
	result := a2a.RecipientObservation{RunID: child.ID, AgentName: req.AgentName}
	switch child.State {
	case run.StateCreated, run.StateRunning, run.StatePaused:
		result.State = a2a.RecipientWaiting
	case run.StateCompleted:
		result.State, result.Output = a2a.RecipientSucceeded, child.Output
	case run.StateFailed, run.StateCancelled, run.StateIncomplete:
		result.State = a2a.RecipientFailed
	default:
		return a2a.RecipientObservation{}, cortex.ErrInvalidContinuation
	}
	return result, nil
}
