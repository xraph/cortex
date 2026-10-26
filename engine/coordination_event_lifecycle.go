package engine

import "github.com/xraph/cortex/coordination"

// WithCoordinationEvents installs bounded private transition delivery. The host
// supplies current source and sink grants and an authenticated idempotent sink.
// Event sequences order index materialization, not historical source occurrence.
func WithCoordinationEvents(config coordination.EventServiceConfig) Option {
	return func(e *Engine) error {
		if e.coordinationEvents != nil {
			return coordination.ErrInvalidScheduler
		}
		applied := config
		report := applied.ReportError
		applied.ReportError = func(err error) {
			e.logError("coordination event delivery failed", err)
			if report != nil {
				report(err)
			}
		}
		service, err := coordination.NewEventService(applied)
		if err != nil {
			return err
		}
		e.coordinationEvents = service
		return nil
	}
}

// CoordinationEventStatus reports local delivery counters and configuration.
func (e *Engine) CoordinationEventStatus() (coordination.TaskServiceStatus, bool) {
	if e.coordinationEvents == nil {
		return coordination.TaskServiceStatus{}, false
	}
	return e.coordinationEvents.Status(), true
}
