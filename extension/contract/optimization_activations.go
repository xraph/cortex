package contract

import (
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	"github.com/xraph/cortex/optimization/operator"
)

func (s *service) bindOptimizationActivationCommands(d *dispatcher.Dispatcher) error {
	commands := operator.Commands{Optimizer: s.deps.Optimization}
	for _, bind := range []func() error{
		func() error {
			return optimizationCommand(d, s, "optimization.activations.activate", "optimization.activate", func(in operator.ActivateInput) string { return in.AgentID }, commands.Activate)
		},
		func() error {
			return optimizationCommand(d, s, "optimization.activations.rollback", "optimization.activate", func(in operator.RollbackInput) string { return in.AgentID }, commands.Rollback)
		},
		func() error {
			return optimizationCommand(d, s, "optimization.activations.deactivate", "optimization.activate", func(in operator.ActivationRouteInput) string { return in.AgentID }, commands.Deactivate)
		},
	} {
		if err := bind(); err != nil {
			return err
		}
	}
	return nil
}
