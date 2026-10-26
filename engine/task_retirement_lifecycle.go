package engine

import "github.com/xraph/cortex/coordination"

// WithTaskRetirement installs guarded retirement of unsuccessful task ownership.
// The host supplies authorized discovery, exact retained accounting and permanent
// worker/tool quiescence proof. Replacement work still requires separate scheduler
// admission. Shutdown joins all discovery, proof and storage callbacks.
func WithTaskRetirement(config coordination.TaskRetirementServiceConfig) Option {
	return func(e *Engine) error {
		if e.taskRetirement != nil {
			return coordination.ErrInvalidScheduler
		}
		applied := config
		report := applied.ReportError
		applied.ReportError = func(err error) {
			e.logError("task retirement operation failed", err)
			if report != nil {
				report(err)
			}
		}
		service, err := coordination.NewTaskRetirementService(applied)
		if err != nil {
			return err
		}
		e.taskRetirement = service
		return nil
	}
}

// TaskRetirementStatus reports local counters and whether retirement is configured.
func (e *Engine) TaskRetirementStatus() (coordination.TaskServiceStatus, bool) {
	if e.taskRetirement == nil {
		return coordination.TaskServiceStatus{}, false
	}
	return e.taskRetirement.Status(), true
}
