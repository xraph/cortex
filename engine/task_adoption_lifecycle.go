package engine

import "github.com/xraph/cortex/coordination"

// WithTaskAdoption installs optional guarded adoption of sealed recovering runs.
// The host supplies durable storage, authorized discovery and snapshot access,
// proof resolution, authentication and fencing. This service can complete a
// recovered claim; it never invokes tasks or settles shared reservations.
func WithTaskAdoption(config coordination.TaskAdoptionServiceConfig) Option {
	return func(e *Engine) error {
		if e.taskAdoption != nil {
			return coordination.ErrInvalidScheduler
		}
		applied := config
		report := applied.ReportError
		applied.ReportError = func(err error) {
			e.logError("task adoption operation failed", err)
			if report != nil {
				report(err)
			}
		}
		service, err := coordination.NewTaskAdoptionService(applied)
		if err != nil {
			return err
		}
		e.taskAdoption = service
		return nil
	}
}

// TaskAdoptionStatus reports local counters and whether adoption is configured.
func (e *Engine) TaskAdoptionStatus() (coordination.TaskServiceStatus, bool) {
	if e.taskAdoption == nil {
		return coordination.TaskServiceStatus{}, false
	}
	return e.taskAdoption.Status(), true
}
