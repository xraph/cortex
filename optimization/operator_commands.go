package optimization

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/xraph/cortex/id"
)

// CommandRequest identifies a lifecycle command independently of a connection.
// Reuse a key only with the same action, revision and authenticated actor for
// this experiment. Retries return its current state without another transition.
type CommandRequest struct {
	ExperimentID     id.ID  `json:"experiment_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	IdempotencyKey   string `json:"idempotency_key"`
}

// StartCommand queues a frozen draft and records the caller's idempotency key.
// It never runs a controller or trial in the requesting goroutine.
func (o *Optimizer) StartCommand(ctx context.Context, request CommandRequest) (Experiment, error) {
	return o.operatorCommand(ctx, request, ActionStart, StateQueued)
}

// CancelCommand cancels pending work and permits an identical recovery retry.
func (o *Optimizer) CancelCommand(ctx context.Context, request CommandRequest) (Experiment, error) {
	return o.operatorCommand(ctx, request, ActionCancel, StateCancelled)
}

// ResumeCommand rechecks the original requester before queuing a paused experiment.
func (o *Optimizer) ResumeCommand(ctx context.Context, request CommandRequest) (Experiment, error) {
	return o.operatorCommand(ctx, request, ActionResume, StateQueued)
}

func (o *Optimizer) operatorCommand(ctx context.Context, request CommandRequest, action Action, next State) (Experiment, error) {
	if err := allowOptimizationInvocation(ctx); err != nil {
		return Experiment{}, err
	}
	change, err := operatorTransition(request, action, next)
	if err != nil {
		return Experiment{}, err
	}
	return o.executeCommand(ctx, change, action)
}

func operatorTransition(request CommandRequest, action Action, next State) (Transition, error) {
	if _, err := ParseID(request.ExperimentID.String(), PrefixExperiment); err != nil {
		return Transition{}, err
	}
	if request.ExpectedRevision < 1 {
		return Transition{}, ErrInvalidSpec
	}
	key, err := operatorCommandKey(request.IdempotencyKey)
	if err != nil {
		return Transition{}, err
	}
	return Transition{ExperimentID: request.ExperimentID, ExpectedRevision: request.ExpectedRevision, NextState: next, IdempotencyKey: key, CommandAction: action}, nil
}

func operatorCommandKey(value string) (string, error) {
	if !ValidOpaqueKey(value) {
		return "", ErrInvalidSpec
	}
	// Keep caller-provided keys out of persistent metadata. The namespace also
	// separates them from existing host and worker transition keys.
	key := fmt.Sprintf("operator/%x", sha256.Sum256([]byte(value)))
	return key, nil
}
