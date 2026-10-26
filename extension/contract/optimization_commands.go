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
		if err := optimizationCommand(d, s, "optimization.experiments."+binding.action, "optimization.run", func(in operator.LifecycleInput) string { return in.ID }, binding.invoke); err != nil {
			return err
		}
	}
	if err := optimizationCommand(d, s, "optimization.decisions.retain", "optimization.manage", func(in operator.RetainInput) string { return in.ID }, commands.Retain); err != nil {
		return err
	}
	return s.bindOptimizationActivationCommands(d)
}

func optimizationCommand[I, O any](d *dispatcher.Dispatcher, s *service, name, permission string, resource func(I) string, invoke func(context.Context, I, string) (O, error)) error {
	return dispatcher.RegisterCommand(d, ContributorName, name, 1, func(ctx context.Context, in I, p dc.Principal) (O, error) {
		var out O
		ctx, err := s.context(ctx, p, permission)
		if err != nil {
			return out, err
		}
		key := dispatcher.CommandIdempotencyKey(ctx)
		if !opt.ValidOpaqueKey(key) {
			return out, optimizationError(opt.ErrInvalidSpec)
		}
		err = s.write(ctx, p, name, resource(in), func() error {
			var callErr error
			out, callErr = invoke(ctx, in, key)
			return optimizationError(callErr)
		})
		return out, err
	}, dispatcher.BypassIdempotency())
}
