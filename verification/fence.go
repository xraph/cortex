package verification

import (
	"context"
	"fmt"
	"sync"
)

// withBindingFence owns callback lifetime for commits and content reads. A broken
// host cannot turn a retained, repeated or escaped callback into a successful result.
func withBindingFence[T any](ctx context.Context, fence CommitFence, binding Binding, action func(context.Context, Binding) (T, error)) (T, error) {
	var zero T
	expected, err := cloneBinding(binding)
	if err != nil {
		return zero, err
	}
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	active, started, finished := true, false, false
	var result T
	var callbackErr, protocolErr error
	fenceErr := fence.WithCommitFence(attemptCtx, expected, func(current Binding) error {
		mu.Lock()
		if !active || started {
			protocolErr = ErrInvalid
			mu.Unlock()
			return ErrInvalid
		}
		started = true
		mu.Unlock()
		value, actionErr := action(attemptCtx, current)
		mu.Lock()
		if !active {
			value = zero
			if actionErr == nil {
				actionErr = ErrInvalid
			}
		}
		result, callbackErr, finished = value, actionErr, true
		mu.Unlock()
		return actionErr
	})
	mu.Lock()
	active = false
	done := started && finished
	value, resultErr, invalid := result, callbackErr, protocolErr
	mu.Unlock()
	cancel()
	if fenceErr != nil {
		return zero, fmt.Errorf("cortex: result fence: %w", fenceErr)
	}
	if invalid != nil {
		return zero, invalid
	}
	if !done {
		return zero, ErrInvalid
	}
	if resultErr != nil {
		return zero, resultErr
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	return value, nil
}
