package engine

import "github.com/xraph/cortex/coordination"

// WithTaskCompletion installs bounded, guarded review and task acceptance work.
// The host supplies current authority and durable discovery. Shutdown joins all
// callbacks before the engine releases its extensions.
func WithTaskCompletion(config coordination.TaskCompletionServiceConfig) Option {
	return func(e *Engine) error {
		if e.taskCompletion != nil {
			return coordination.ErrInvalidScheduler
		}
		applied := config
		report := applied.ReportError
		applied.ReportError = func(err error) {
			e.logError("task completion operation failed", err)
			if report != nil {
				report(err)
			}
		}
		service, err := coordination.NewTaskCompletionService(applied)
		if err != nil {
			return err
		}
		e.taskCompletion = service
		return nil
	}
}

// TaskCompletionStatus reports bounded local counters and configuration presence.
func (e *Engine) TaskCompletionStatus() (coordination.TaskServiceStatus, bool) {
	if e.taskCompletion == nil {
		return coordination.TaskServiceStatus{}, false
	}
	return e.taskCompletion.Status(), true
}
