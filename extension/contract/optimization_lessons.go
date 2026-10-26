package contract

import (
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	"github.com/xraph/cortex/optimization/operator"
)

func (s *service) bindOptimizationLessonCommands(d *dispatcher.Dispatcher) error {
	commands := operator.Commands{Optimizer: s.deps.Optimization}
	if err := optimizationCommand(d, s, "optimization.lessons.retry", "optimization.manage", func(in operator.RetryLessonInput) string { return in.ID }, commands.RetryLesson); err != nil {
		return err
	}
	return optimizationCommand(d, s, "optimization.lessons.revoke", "optimization.manage", func(in operator.RevokeLessonInput) string { return in.ID }, commands.RevokeLesson)
}
