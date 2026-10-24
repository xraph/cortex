package coordination

import (
	"context"
	"time"

	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/internal/hostport"
)

// EffectRecoveryServiceConfig connects authorized plan discovery to sealed
// recovering runs. Recovery.Store must also implement BoundEffectStore,
// EffectDiscoveryStore, RunCallSealStore and ListTaskClaims. Discovery must
// authorize claim reads and include outstanding claims in cancelled plans.
// Scan and per-operation authority are required. Callbacks must honor cancellation.
type EffectRecoveryServiceConfig struct {
	Recovery         execution.EffectExecutorConfig
	Discovery        TaskPlanSource
	Interval         time.Duration
	DiscoveryTimeout time.Duration
	PageTimeout      time.Duration
	PageSize         int
	MaxInFlight      int
	MaxPerScope      int
	ReportError      func(error)
}

// EffectRecoveryService owns discovery and read-only effect reconciliation.
// It never seals runs, dispatches tools, adopts results or refunds reservations.
// Restart scans from the beginning and reuses durable terminal receipts.
type EffectRecoveryService struct{ service *TaskService }

// NewEffectRecoveryService validates ports without starting work. Defaults are
// four concurrent plans, one per scope, 64 entries per page, five seconds per
// claim/seal read and one second between pages. Each effect page is bounded by
// execution.MaxEffectDuration. Recovery.MaxConcurrent limits scans and probes.
func NewEffectRecoveryService(config EffectRecoveryServiceConfig) (*EffectRecoveryService, error) {
	storage, ok := config.Recovery.Store.(recoveringRunStore)
	if !ok || hostport.IsNil(storage) || config.Recovery.AuthorizeScan == nil {
		return nil, ErrInvalidScheduler
	}
	if _, supported := config.Recovery.Store.(execution.BoundEffectStore); !supported {
		return nil, ErrInvalidScheduler
	}
	if _, supported := config.Recovery.Store.(execution.EffectDiscoveryStore); !supported {
		return nil, ErrInvalidScheduler
	}
	config.Recovery.Adapters = append([]execution.EffectRegistration(nil), config.Recovery.Adapters...)
	if _, err := execution.NewEffectExecutor(config.Recovery); err != nil {
		return nil, err
	}
	service, err := newRunRecoveryService(runRecoveryConfig{Discovery: config.Discovery, Interval: config.Interval, DiscoveryTimeout: config.DiscoveryTimeout, PageTimeout: config.PageTimeout, PageSize: config.PageSize, MaxInFlight: config.MaxInFlight, MaxPerScope: config.MaxPerScope, ReportError: config.ReportError}, storage, func() (runRecoveryPage, error) {
		executor, makeErr := execution.NewEffectExecutor(config.Recovery)
		if makeErr != nil {
			return nil, makeErr
		}
		return func(ctx context.Context, seal execution.RunCallSeal, after string, limit int) (string, error) {
			page, scanErr := executor.RecoverSealedPage(ctx, execution.EffectRecoveryPageRequest{Seal: seal, After: after, Limit: limit})
			return page.NextAfter, scanErr
		}, nil
	})
	if err != nil {
		return nil, err
	}
	return &EffectRecoveryService{service: service}, nil
}

// Start preserves service identity values and owns an independent lifetime.
func (s *EffectRecoveryService) Start(ctx context.Context) error { return s.service.Start(ctx) }

// RequestStop signals cancellation without claiming drainage.
func (s *EffectRecoveryService) RequestStop() { s.service.RequestStop() }

// Stop joins discovery, storage reads and adapter callbacks before returning.
func (s *EffectRecoveryService) Stop(ctx context.Context) error { return s.service.Stop(ctx) }

// Status returns local counters without scope, run or provider content.
func (s *EffectRecoveryService) Status() TaskServiceStatus { return s.service.Status() }
