package coordination

import (
	"context"
	"time"

	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/internal/hostport"
)

// ProviderRecoveryServiceConfig connects authorized plan discovery to retained
// recovering claims. Recovery.Store must also provide ListTaskClaims and
// RunCallSealStore. Discovery authorizes plan/claim reads under the service
// identity and must include outstanding claims in cancelled or completed plans.
// Recovery.AuthorizeScan and every per-call authority port remain mandatory.
// Unsealed runs are left untouched for a later pass. The service cannot seal a
// journal, execute calls, adopt a result, or settle usage.
type ProviderRecoveryServiceConfig struct {
	Recovery         execution.ProviderRecoveryConfig
	Discovery        TaskPlanSource
	Interval         time.Duration
	DiscoveryTimeout time.Duration
	PageTimeout      time.Duration
	PageSize         int
	MaxInFlight      int
	MaxPerScope      int
	ReportError      func(error)
}

// ProviderRecoveryService owns plan discovery, claim pages and provider recovery
// pages until Stop drains them. Restart begins discovery again and reuses durable
// provider receipts. Scope fairness and lifecycle behavior match TaskService.
type ProviderRecoveryService struct{ service *TaskService }

// NewProviderRecoveryService validates ports without starting work. Defaults are
// four concurrent plan passes, one per scope, 64 entries per page, five seconds
// per claim/seal read and one second between pages. Provider pages use Recovery's
// own timeout. A pass retains its slot through all claim and call pages.
func NewProviderRecoveryService(config ProviderRecoveryServiceConfig) (*ProviderRecoveryService, error) {
	storage, ok := config.Recovery.Store.(recoveringRunStore)
	if !ok || hostport.IsNil(storage) || config.Recovery.AuthorizeScan == nil {
		return nil, ErrInvalidScheduler
	}
	if _, err := execution.NewProviderReconciler(config.Recovery); err != nil {
		return nil, err
	}
	service, err := newRunRecoveryService(runRecoveryConfig{Discovery: config.Discovery, Interval: config.Interval, DiscoveryTimeout: config.DiscoveryTimeout, PageTimeout: config.PageTimeout, PageSize: config.PageSize, MaxInFlight: config.MaxInFlight, MaxPerScope: config.MaxPerScope, ReportError: config.ReportError}, storage, func() (runRecoveryPage, error) {
		reconciler, makeErr := execution.NewProviderReconciler(config.Recovery)
		if makeErr != nil {
			return nil, makeErr
		}
		return func(ctx context.Context, seal execution.RunCallSeal, after string, limit int) (string, error) {
			page, scanErr := reconciler.ReconcilePage(ctx, execution.ProviderRecoveryPageRequest{Seal: seal, After: after, Limit: limit})
			return page.NextAfter, scanErr
		}, nil
	})
	if err != nil {
		return nil, err
	}
	return &ProviderRecoveryService{service: service}, nil
}

// Start preserves service identity values and owns an independent lifetime.
func (s *ProviderRecoveryService) Start(ctx context.Context) error { return s.service.Start(ctx) }

// RequestStop signals cancellation without claiming drainage.
func (s *ProviderRecoveryService) RequestStop() { s.service.RequestStop() }

// Stop joins discovery, storage reads and provider callbacks before returning.
func (s *ProviderRecoveryService) Stop(ctx context.Context) error { return s.service.Stop(ctx) }

// Status reports local counters without scope, run or provider content.
func (s *ProviderRecoveryService) Status() TaskServiceStatus { return s.service.Status() }
