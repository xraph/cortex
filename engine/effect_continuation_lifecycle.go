package engine

import "github.com/xraph/cortex/coordination"

// WithEffectContinuation installs bounded retries and explicit compensation for
// sealed recovering tasks. The host must authenticate provider dispositions and
// current grants. Results remain private evidence for a new verified preparation;
// this service never accepts the original task or dispatches its replacement.
func WithEffectContinuation(config coordination.EffectContinuationServiceConfig) Option {
	return func(e *Engine) error {
		if e.effectContinuation != nil {
			return coordination.ErrInvalidScheduler
		}
		applied := config
		report := applied.ReportError
		applied.ReportError = func(err error) {
			e.logError("effect continuation operation failed", err)
			if report != nil {
				report(err)
			}
		}
		service, err := coordination.NewEffectContinuationService(applied)
		if err != nil {
			return err
		}
		e.effectContinuation = service
		return nil
	}
}

// EffectContinuationStatus returns local counters and whether recovery is configured.
func (e *Engine) EffectContinuationStatus() (coordination.TaskServiceStatus, bool) {
	if e.effectContinuation == nil {
		return coordination.TaskServiceStatus{}, false
	}
	return e.effectContinuation.Status(), true
}
