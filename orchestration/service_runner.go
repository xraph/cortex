package orchestration

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
)

// persistingRunner serializes evidence writes without serializing agent work.
// An acknowledgment failure closes admission even if a later write succeeds.
type persistingRunner struct {
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
		if slices.Contains(r.record.AgentRunIDs, inv.RunID) {
			reserveErr = errors.Join(reserveErr, cortex.ErrRunIncomplete)
		} else {
			r.record.AgentRunIDs = append(r.record.AgentRunIDs, inv.RunID)
			reserveErr = errors.Join(reserveErr, persistOrchestrationRun(ctx, r.runs, r.record))
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
	result.Err = errors.Join(result.Err, runErr, r.writeErr)
	return result, result.Err
}
