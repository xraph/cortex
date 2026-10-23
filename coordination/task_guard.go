package coordination

import (
	"context"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/internal/hostport"
)

type frozenTaskRequest[T any] interface{ Freeze() (T, string, error) }

type taskAuthority[T any] struct {
	operation string
	invalid   error
	conflict  error
	scope     func(T) cortex.Scope
	fence     func(context.Context, T, func(T) error) error
	authorize func(context.Context, T) error
}

// withTaskAuthority keeps identity, callback lifetime and authorization ordering
// identical for task admission, dispatch and completion. Ports receive detached requests.
func withTaskAuthority[T frozenTaskRequest[T], R any](ctx context.Context, request T, guard taskAuthority[T], commit func(context.Context, T, string) (R, error)) (R, error) {
	var zero R
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return zero, err
	}
	frozen, digest, err := request.Freeze()
	if err != nil {
		return zero, err
	}
	if guard.scope(frozen).Canonical() != scope.Canonical() {
		return zero, guard.invalid
	}
	fenceInput, _, err := frozen.Freeze()
	if err != nil {
		return zero, err
	}
	return hostport.WithSynchronousFence(ctx, guard.invalid, func(attemptCtx context.Context, next func(T) error) error {
		if fenceErr := guard.fence(attemptCtx, fenceInput, next); fenceErr != nil {
			return fmt.Errorf("cortex: %s fence: %w", guard.operation, fenceErr)
		}
		return nil
	}, func(attemptCtx context.Context, current T) (R, error) {
		if contextErr := attemptCtx.Err(); contextErr != nil {
			return zero, contextErr
		}
		authorized, currentDigest, freezeErr := current.Freeze()
		if freezeErr != nil {
			return zero, freezeErr
		}
		if currentDigest != digest {
			return zero, guard.conflict
		}
		if authorizeErr := guard.authorize(attemptCtx, authorized); authorizeErr != nil {
			return zero, fmt.Errorf("cortex: authorize %s: %w", guard.operation, authorizeErr)
		}
		if contextErr := attemptCtx.Err(); contextErr != nil {
			return zero, contextErr
		}
		return commit(attemptCtx, frozen, digest)
	})
}
