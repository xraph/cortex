package engine

import "github.com/xraph/cortex/coordination"

// WithEffectRecovery installs optional discovery of sealed recovering runs.
// The host supplies authorized discovery, the durable recovery store and every
// effect authority port. This service never closes journals, dispatches effects,
// accepts tasks, releases claims or refunds shared reservations.
func WithEffectRecovery(config coordination.EffectRecoveryServiceConfig) Option {
	return func(e *Engine) error {
		if e.effectRecovery != nil {
			return coordination.ErrInvalidScheduler
		}
		applied := config
		report := applied.ReportError
		applied.ReportError = func(err error) {
			e.logError("effect recovery operation failed", err)
			if report != nil {
				report(err)
			}
		}
		service, err := coordination.NewEffectRecoveryService(applied)
		if err != nil {
			return err
		}
		e.effectRecovery = service
		return nil
	}
}

// EffectRecoveryStatus returns local counters and whether recovery is configured.
func (e *Engine) EffectRecoveryStatus() (coordination.TaskServiceStatus, bool) {
	if e.effectRecovery == nil {
		return coordination.TaskServiceStatus{}, false
	}
	return e.effectRecovery.Status(), true
}
