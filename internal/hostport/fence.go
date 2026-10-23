package hostport

import (
	"context"
	"sync"
)

// WithSynchronousFence owns one callback's lifetime across a trusted host guard.
// It rejects missing, repeated and escaped callbacks and cancels unfinished work.
// The host must hold real authority/revision guards until the callback completes.
func WithSynchronousFence[I, O any](ctx context.Context, invalidCallback error, invoke func(context.Context, func(I) error) error, action func(context.Context, I) (O, error)) (O, error) {
	var zero O
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	active, started, finished := true, false, false
	var result O
	var callbackErr, protocolErr error
	fenceErr := invoke(attemptCtx, func(current I) error {
		mu.Lock()
		if !active || started {
			protocolErr = invalidCallback
			mu.Unlock()
			return invalidCallback
		}
		started = true
		mu.Unlock()
		value, actionErr := action(attemptCtx, current)
		mu.Lock()
		if !active {
			value = zero
			if actionErr == nil {
				actionErr = invalidCallback
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
		return zero, fenceErr
	}
	if invalid != nil {
		return zero, invalid
	}
	if !done {
		return zero, invalidCallback
	}
	if resultErr != nil {
		return zero, resultErr
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	return value, nil
}
