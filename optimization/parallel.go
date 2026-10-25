package optimization

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
)

// trialBatchError keeps successful sibling updates eligible for a fenced commit.
// Even a budget failure must not close the experiment before those results persist.
type trialBatchError struct{ cause error }

func (e *trialBatchError) Error() string { return e.cause.Error() }
func (e *trialBatchError) Unwrap() error { return e.cause }

func parallelTrials(trials []Trial, phase Phase, limit int) ([]Trial, bool) {
	active := 0
	var running, captured, queued []Trial
	for _, trial := range trials {
		for _, attempt := range trial.Attempts {
			if attempt.Execution == nil && (attempt.Stop == nil || attempt.Stop.State == StopUnknown) {
				active++
			}
		}
		if !trialPending(trial, phase) {
			continue
		}
		switch {
		case trial.Execution != nil:
			captured = append(captured, trial)
		case len(trial.Attempts) > 0:
			running = append(running, trial)
		default:
			queued = append(queued, trial)
		}
	}
	pending := len(running)+len(captured)+len(queued) > 0
	// Listing order and completion timing do not choose the next admitted trial.
	for _, group := range [][]Trial{running, captured, queued} {
		slices.SortFunc(group, func(a, b Trial) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	}
	running = append(running, captured...)
	batch := running
	if len(batch) > limit {
		batch = batch[:limit]
	}
	free := min(limit-active, limit-len(batch), len(queued))
	if free > 0 {
		batch = append(batch, queued[:free]...)
	}
	return batch, pending
}

func (h *TrialPhaseHandler) parallelStep(ctx context.Context, item WorkItem, trials []Trial) (Transition, error) {
	if len(trials) == 0 {
		return Transition{}, ErrUnknownAttempt
	}
	type result struct {
		change Transition
		err    error
	}
	results := make([]result, len(trials))
	var workers sync.WaitGroup
	for i, trial := range trials {
		workers.Go(func() { results[i].change, results[i].err = h.step(ctx, item, trial) })
	}
	workers.Wait()
	change := Transition{NextState: StateRunning, NextPhase: item.Experiment.Phase}
	var failures []error
	budgetOnly := true
	for _, result := range results {
		if result.err != nil {
			failures = append(failures, result.err)
			budgetOnly = budgetOnly && errors.Is(result.err, ErrBudgetExceeded) && !errors.Is(result.err, ErrUnknownAttempt)
			continue
		}
		change.TrialUpdates = append(change.TrialUpdates, result.change.TrialUpdates...)
		change.Evaluations = append(change.Evaluations, result.change.Evaluations...)
	}
	if len(failures) == 0 {
		return change, nil
	}
	err := errors.Join(failures...)
	if len(change.TrialUpdates)+len(change.Evaluations) == 0 && budgetOnly {
		return Transition{}, err
	}
	return change, &trialBatchError{cause: err}
}
