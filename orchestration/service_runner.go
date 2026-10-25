package orchestration

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
)

// persistingRunner serializes evidence writes without serializing agent work.
// An acknowledgment failure closes admission even if a later write succeeds.
type persistingRunner struct {
	observer ChildObserver
	runner   AgentRunner
	reserved ReservedAgentRunner
	runs     RunStore
	record   *Run
	mu       sync.Mutex
	writeErr error
}

func (r *persistingRunner) RunAgent(ctx context.Context, name, input string, opts *RunOpts) (*AgentResult, error) {
	r.mu.Lock()
	admissionErr := r.writeErr
	r.mu.Unlock()
	if admissionErr != nil {
		return nil, admissionErr
	}
	if err := CheckCoordinator(ctx); err != nil {
		return nil, err
	}
	if r.observer != nil {
		result, replayed, err := r.replay(ctx, name, input, opts)
		if replayed {
			return result, err
		}
	}
	if !hostport.IsNil(r.reserved) {
		return r.runReserved(ctx, name, input, opts)
	}
	result, runErr := r.runner.RunAgent(ctx, name, input, opts)
	if result == nil || result.RunID.IsNil() {
		return result, runErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record.AgentRunIDs = append(r.record.AgentRunIDs, result.RunID)
	if err := persistOrchestrationRun(ctx, r.runs, r.record); err != nil {
		r.writeErr = errors.Join(r.writeErr, err)
	}
	return result, errors.Join(runErr, r.writeErr)
}

func (r *persistingRunner) runReserved(ctx context.Context, name, input string, opts *RunOpts) (*AgentResult, error) {
	journal, journaled := r.runs.(InvocationStore)
	var intent InvocationIntent
	if journaled {
		var intentErr error
		intent, intentErr = invocationIntent(ctx, r.record.ID, name, input, opts)
		if intentErr == nil {
			if pass := coordinatorFromContext(ctx); pass != nil {
				intentErr = pass.begin(ctx, intent)
			} else {
				intentErr = journal.BeginOrchestrationInvocation(ctx, intent)
			}
		}
		if intentErr != nil {
			r.mu.Lock()
			r.writeErr = errors.Join(r.writeErr, intentErr)
			r.mu.Unlock()
			return &AgentResult{AgentName: name, Err: intentErr}, intentErr
		}
	}
	invocation, reserveErr := r.reserved.ReserveAgent(ctx, r.record.ID, name, input, opts)
	var inv ReservedInvocation
	if invocation != nil {
		inv = *invocation
	}
	result := &AgentResult{AgentName: name}
	r.mu.Lock()
	validID := !inv.RunID.IsNil() && inv.RunID.Prefix() == id.PrefixAgentRun
	if validID {
		result.RunID = inv.RunID
		if journaled {
			writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			reserveErr = errors.Join(reserveErr, journal.BindOrchestrationInvocation(writeCtx, intent, inv.RunID))
			cancel()
		}
		if slices.Contains(r.record.AgentRunIDs, inv.RunID) {
			reserveErr = errors.Join(reserveErr, cortex.ErrRunIncomplete)
		} else {
			r.record.AgentRunIDs = append(r.record.AgentRunIDs, inv.RunID)
			reserveErr = errors.Join(reserveErr, r.persistChild(ctx, inv.RunID))
		}
	}
	if !validID || inv.Execute == nil {
		reserveErr = errors.Join(reserveErr, cortex.ErrRunIncomplete)
	}
	r.writeErr = errors.Join(r.writeErr, reserveErr, ctx.Err())
	admissionErr := r.writeErr
	r.mu.Unlock()
	if admissionErr != nil {
		result.Err = admissionErr
		return result, admissionErr
	}

	if err := CheckCoordinator(ctx); err != nil {
		return result, err
	}
	actual, runErr := inv.Execute(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case actual == nil:
		if runErr == nil {
			runErr = cortex.ErrRunIncomplete
		}
	case actual.RunID != inv.RunID || actual.AgentName != name:
		// Preserve unexpected identities for reconciliation, without accepting
		// their output as the result of the reserved child.
		if !actual.RunID.IsNil() && actual.RunID.Prefix() == id.PrefixAgentRun && !slices.Contains(r.record.AgentRunIDs, actual.RunID) {
			r.record.AgentRunIDs = append(r.record.AgentRunIDs, actual.RunID)
			r.writeErr = errors.Join(r.writeErr, persistOrchestrationRun(ctx, r.runs, r.record))
		}
		r.writeErr = errors.Join(r.writeErr, cortex.ErrRunIncomplete)
		runErr = errors.Join(runErr, actual.Err)
	default:
		*result = *actual
	}
	if r.observer != nil && journaled && runErr == nil && r.writeErr == nil {
		r.writeErr = errors.Join(r.writeErr, intent.ValidateResult(result))
		storage, ok := r.runs.(ContinuationStore)
		if !ok {
			r.writeErr = errors.Join(r.writeErr, cortex.ErrOrchestrationContinuationUnavailable)
		} else if r.writeErr == nil {
			r.writeErr = errors.Join(r.writeErr, storage.PutOrchestrationInvocationResult(ctx, intent, *result))
		}
	}
	result.Err = errors.Join(result.Err, runErr, r.writeErr)
	return result, result.Err
}

func (r *persistingRunner) persistChild(ctx context.Context, child id.AgentRunID) error {
	if coordinatorFromContext(ctx) != nil {
		// Guarded reservation already linked this identity in its transaction.
		stored, err := r.runs.GetOrchestrationRun(ctx, r.record.ID)
		if err != nil {
			return err
		}
		if !slices.Contains(stored.AgentRunIDs, child) {
			return cortex.ErrRunIncomplete
		}
		return CheckCoordinator(ctx)
	}
	links, ok := r.runs.(ChildRunStore)
	if !ok {
		// Legacy stores retain the service's serialized snapshot behavior.
		// Asynchronous admission requires the atomic capability explicitly.
		return persistOrchestrationRun(ctx, r.runs, r.record)
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return links.LinkOrchestrationChild(writeCtx, r.record.ID, child)
}
