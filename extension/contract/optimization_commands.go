package contract

import (
	"context"

	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/operator"
)

func (s *service) bindOptimizationCommands(d *dispatcher.Dispatcher) error {
	commands := operator.Commands{Optimizer: s.deps.Optimization}
	for _, binding := range []struct {
		action string
		invoke func(context.Context, operator.LifecycleInput, string) (opt.Experiment, error)
	}{
		{"start", commands.Start},
		{"cancel", commands.Cancel},
		{"resume", commands.Resume},
	} {
		if err := optimizationCommand(d, s, "optimization.experiments."+binding.action, binding.invoke); err != nil {
			return err
		}
	}
	return nil
}

func optimizationCommand(d *dispatcher.Dispatcher, s *service, name string, invoke func(context.Context, operator.LifecycleInput, string) (opt.Experiment, error)) error {
	return dispatcher.RegisterCommand(d, ContributorName, name, 1, func(ctx context.Context, in operator.LifecycleInput, p dc.Principal) (opt.Experiment, error) {
		var out opt.Experiment
		ctx, err := s.context(ctx, p, "optimization.run")
		if err != nil {
			return out, err
		}
		key := dispatcher.CommandIdempotencyKey(ctx)
		if !opt.ValidOpaqueKey(key) {
			return out, optimizationError(opt.ErrInvalidSpec)
		}
		err = s.write(ctx, p, name, in.ID, func() error {
			var callErr error
			out, callErr = invoke(ctx, in, key)
			return optimizationError(callErr)
		})
		return out, err
	}, dispatcher.BypassIdempotency())
}
