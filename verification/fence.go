package verification

import (
	"context"
	"fmt"

	"github.com/xraph/cortex/internal/hostport"
)

// withBindingFence owns callback lifetime for commits and content reads. A broken
// host cannot turn a retained, repeated or escaped callback into a successful result.
func withBindingFence[T any](ctx context.Context, fence CommitFence, binding Binding, action func(context.Context, Binding) (T, error)) (T, error) {
	var zero T
	expected, err := cloneBinding(binding)
	if err != nil {
		return zero, err
	}
	return hostport.WithSynchronousFence(ctx, ErrInvalid, func(attemptCtx context.Context, next func(Binding) error) error {
		if fenceErr := fence.WithCommitFence(attemptCtx, expected, next); fenceErr != nil {
			return fmt.Errorf("cortex: result fence: %w", fenceErr)
		}
		return nil
	}, action)
}
