package engine

import "github.com/xraph/cortex/coordination"

// WithPlanReplanning installs bounded, guarded plan proposal and recovery work.
// The host supplies current authority and durable discovery. Shutdown joins all
// callbacks before the engine releases its extensions.
func WithPlanReplanning(config coordination.PlanReplanServiceConfig) Option {
	return func(e *Engine) error {
		if e.planReplanning != nil {
			return coordination.ErrInvalidScheduler
		}
		applied := config
		report := applied.ReportError
		applied.ReportError = func(err error) {
			e.logError("plan replanning operation failed", err)
			if report != nil {
				report(err)
			}
		}
		service, err := coordination.NewPlanReplanService(applied)
		if err != nil {
			return err
		}
		e.planReplanning = service
		return nil
	}
}

// PlanReplanningStatus reports bounded local counters and configuration presence.
func (e *Engine) PlanReplanningStatus() (coordination.TaskServiceStatus, bool) {
	if e.planReplanning == nil {
		return coordination.TaskServiceStatus{}, false
	}
	return e.planReplanning.Status(), true
}
