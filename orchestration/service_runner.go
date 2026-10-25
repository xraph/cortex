package orchestration

import (
	"context"
	"errors"
	"sync"
)

// persistingRunner serializes evidence writes without serializing agent work.
// An acknowledgment failure closes admission even if a later write succeeds.
type persistingRunner struct {
	runner   AgentRunner
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
