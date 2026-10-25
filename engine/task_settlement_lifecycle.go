package engine

import "github.com/xraph/cortex/coordination"

// WithTaskSettlement installs optional accounting for accepted task runs.
// The host supplies authorized discovery and reads, closure authority, billing
// resolution and settlement fencing. Separate optional call-seal ports close
// ordinary completed journals. This service never invokes task work.
func WithTaskSettlement(config coordination.TaskSettlementServiceConfig) Option {
	return func(e *Engine) error {
		if e.taskSettlement != nil {
			return coordination.ErrInvalidScheduler
		}
		applied := config
		report := applied.ReportError
		applied.ReportError = func(err error) {
			e.logError("task settlement operation failed", err)
			if report != nil {
				report(err)
			}
		}
		service, err := coordination.NewTaskSettlementService(applied)
		if err != nil {
			return err
		}
		e.taskSettlement = service
		return nil
	}
}

// TaskSettlementStatus reports local counters and whether accounting is configured.
func (e *Engine) TaskSettlementStatus() (coordination.TaskServiceStatus, bool) {
	if e.taskSettlement == nil {
		return coordination.TaskServiceStatus{}, false
	}
	return e.taskSettlement.Status(), true
}
