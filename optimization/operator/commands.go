package operator

import (
	"context"

	opt "github.com/xraph/cortex/optimization"
)

// LifecycleInput contains command payload fields. The idempotency key comes
// separately from the REST header or dashboard envelope, never from this body.
type LifecycleInput struct {
	ID               string `json:"id" path:"id"`
	ExpectedRevision int64  `json:"expected_revision"`
}

// Commands adapts lifecycle requests to durable optimizer commands.
type Commands struct{ Optimizer *opt.Optimizer }

// Start queues a frozen draft without executing trial or controller work.
func (c Commands) Start(ctx context.Context, in LifecycleInput, key string) (opt.Experiment, error) {
	return c.command(ctx, in, key, (*opt.Optimizer).StartCommand)
}

// Cancel invalidates pending work while retaining original accounting evidence.
func (c Commands) Cancel(ctx context.Context, in LifecycleInput, key string) (opt.Experiment, error) {
	return c.command(ctx, in, key, (*opt.Optimizer).CancelCommand)
}

// Resume queues a paused experiment after the original requester's current grants.
func (c Commands) Resume(ctx context.Context, in LifecycleInput, key string) (opt.Experiment, error) {
	return c.command(ctx, in, key, (*opt.Optimizer).ResumeCommand)
}

func (c Commands) command(ctx context.Context, in LifecycleInput, key string, invoke func(*opt.Optimizer, context.Context, opt.CommandRequest) (opt.Experiment, error)) (opt.Experiment, error) {
	if c.Optimizer == nil {
		return opt.Experiment{}, opt.ErrUnavailable
	}
	experimentID, err := opt.ParseID(in.ID, opt.PrefixExperiment)
	if err != nil {
		return opt.Experiment{}, err
	}
	return invoke(c.Optimizer, ctx, opt.CommandRequest{ExperimentID: experimentID, ExpectedRevision: in.ExpectedRevision, IdempotencyKey: key})
}
