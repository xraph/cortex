package engine

import "github.com/xraph/cortex/coordination"

// WithClaimRecovery installs authorized discovery and claim expiry before Start.
// The service owns no provider or tool execution capability. Discovery must also
// include cancelled and completed plans that still retain outstanding claims.
// Supply a store capability explicitly; this option never broadens store scope.
func WithClaimRecovery(config coordination.ClaimRecoveryConfig) Option {
	return func(e *Engine) error {
		if e.claimRecovery != nil {
			return coordination.ErrInvalidScheduler
		}
		applied := config
		report := applied.ReportError
		applied.ReportError = func(err error) {
			e.logError("claim recovery operation failed", err)
			if report != nil {
				report(err)
			}
		}
		service, err := coordination.NewClaimRecoveryService(applied)
		if err != nil {
			return err
		}
		e.claimRecovery = service
		return nil
	}
}

// ClaimRecoveryStatus reports local counters and whether recovery was configured.
func (e *Engine) ClaimRecoveryStatus() (coordination.TaskServiceStatus, bool) {
	if e.claimRecovery == nil {
		return coordination.TaskServiceStatus{}, false
	}
	return e.claimRecovery.Status(), true
}
