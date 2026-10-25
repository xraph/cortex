package engine

import "github.com/xraph/cortex/coordination"

// WithOrchestrationExpiry installs deadline cleanup before Start. Supply an
// explicit authorized source and an expirer backed by the durable budget store.
// The service has no agent execution or reservation settlement capability.
func WithOrchestrationExpiry(config coordination.OrchestrationExpiryConfig) Option {
	return func(e *Engine) error {
		if e.orchestrationExpiry != nil {
			return coordination.ErrInvalidScheduler
		}
		applied := config
		report := applied.ReportError
		applied.ReportError = func(err error) {
			e.logError("orchestration expiry operation failed", err)
			if report != nil {
				report(err)
			}
		}
		service, err := coordination.NewOrchestrationExpiryService(applied)
		if err != nil {
			return err
		}
		e.orchestrationExpiry = service
		return nil
	}
}

// OrchestrationExpiryStatus reports whether cleanup was configured and its local
// counters. Stopping means owned callbacks have not yet drained.
func (e *Engine) OrchestrationExpiryStatus() (coordination.TaskServiceStatus, bool) {
	if e.orchestrationExpiry == nil {
		return coordination.TaskServiceStatus{}, false
	}
	return e.orchestrationExpiry.Status(), true
}
