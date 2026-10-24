package engine

import "github.com/xraph/cortex/coordination"

// WithProviderRecovery installs optional discovery of sealed recovering runs.
// The host supplies authorized discovery, the durable recovery store and every
// provider authority port. This service never closes journals, invokes models or
// tools, accepts tasks, releases claims or refunds shared reservations.
func WithProviderRecovery(config coordination.ProviderRecoveryServiceConfig) Option {
	return func(e *Engine) error {
		if e.providerRecovery != nil {
			return coordination.ErrInvalidScheduler
		}
		applied := config
		report := applied.ReportError
		applied.ReportError = func(err error) {
			e.logError("provider recovery operation failed", err)
			if report != nil {
				report(err)
			}
		}
		service, err := coordination.NewProviderRecoveryService(applied)
		if err != nil {
			return err
		}
		e.providerRecovery = service
		return nil
	}
}

// ProviderRecoveryStatus returns local counters and whether recovery is configured.
func (e *Engine) ProviderRecoveryStatus() (coordination.TaskServiceStatus, bool) {
	if e.providerRecovery == nil {
		return coordination.TaskServiceStatus{}, false
	}
	return e.providerRecovery.Status(), true
}
