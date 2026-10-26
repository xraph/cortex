package operator

import (
	"context"

	opt "github.com/xraph/cortex/optimization"
)

// CreateInput accepts catalog choices, never executable bundles or host policy.
type CreateInput struct {
	Selection opt.CreationSelection `json:"selection"`
}

// Create saves a host-frozen draft using the separate transport command identity.
func (c Commands) Create(ctx context.Context, in CreateInput, key string) (opt.Experiment, error) {
	if c.Optimizer == nil {
		return opt.Experiment{}, opt.ErrUnavailable
	}
	return c.Optimizer.CreateCommand(ctx, opt.CreationRequest{Selection: in.Selection, IdempotencyKey: key})
}
