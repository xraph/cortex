package coordination

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
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

type providerClaimStore interface {
	execution.RunCallSealStore
	ListTaskClaims(context.Context, ClaimListRequest) ([]Claim, error)
}

// NewProviderRecoveryService validates ports without starting work. Defaults are
// four concurrent plan passes, one per scope, 64 entries per page, five seconds
// per claim/seal read and one second between pages. Provider pages use Recovery's
// own timeout. A pass retains its slot through all claim and call pages.
func NewProviderRecoveryService(config ProviderRecoveryServiceConfig) (*ProviderRecoveryService, error) {
	storage, ok := config.Recovery.Store.(providerClaimStore)
	if !ok || hostport.IsNil(storage) || config.Recovery.AuthorizeScan == nil {
		return nil, ErrInvalidScheduler
	}
	if _, err := execution.NewProviderReconciler(config.Recovery); err != nil {
		return nil, err
	}
	if config.PageSize == 0 {
		config.PageSize = 64
	}
	if config.PageTimeout == 0 {
		config.PageTimeout = 5 * time.Second
	}
	if config.MaxInFlight == 0 {
		config.MaxInFlight = 4
	}
	if config.MaxPerScope == 0 {
		config.MaxPerScope = 1
	}
	if config.Interval == 0 {
		config.Interval = time.Second
	}
	if config.PageSize < 1 || config.PageSize > 256 || config.PageTimeout < time.Millisecond || config.PageTimeout > time.Minute || config.MaxPerScope < 1 || config.MaxPerScope > 4096 {
		return nil, ErrInvalidScheduler
	}
	service, err := newPlanService(TaskServiceConfig{Discovery: config.Discovery, Interval: config.Interval, DiscoveryTimeout: config.DiscoveryTimeout, MaxInFlight: config.MaxInFlight, ReportError: config.ReportError, Scheduler: SchedulerConfig{MaxPerScope: config.MaxPerScope}}, func() (serviceScheduler, error) {
		reconciler, makeErr := execution.NewProviderReconciler(config.Recovery)
		if makeErr != nil {
			return nil, makeErr
		}
		return &providerRecoveryScheduler{storage: storage, reconciler: reconciler, config: config}, nil
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

type providerRecoveryScheduler struct {
	storage    providerClaimStore
	reconciler *execution.ProviderReconciler
	config     ProviderRecoveryServiceConfig
}

func (s *providerRecoveryScheduler) Tick(ctx context.Context, planID id.ID) error {
	request := ClaimListRequest{PlanID: planID, Limit: s.config.PageSize}
	// Keep only one error for this failed pass. A run-specific denial or outage
	// must not starve later claims or accumulate unbounded error data.
	var firstFailure error
	for {
		claims, err := s.claimPage(ctx, request)
		if err != nil {
			return err
		}
		for _, claim := range claims {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			if claim.State == ClaimRecovering {
				if recoveryErr := s.recoverRun(ctx, claim); recoveryErr != nil {
					if firstFailure == nil {
						firstFailure = recoveryErr
					}
				}
			}
		}
		if len(claims) < request.Limit {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			return firstFailure
		}
		request.AfterTaskID = claims[len(claims)-1].TaskID
		if waitErr := waitRecoveryPage(ctx, s.config.Interval); waitErr != nil {
			return waitErr
		}
	}
}
func (s *providerRecoveryScheduler) claimPage(ctx context.Context, request ClaimListRequest) ([]Claim, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	bounded, cancel := context.WithTimeout(ctx, s.config.PageTimeout)
	defer cancel()
	claims, err := s.storage.ListTaskClaims(bounded, request)
	if contextErr := bounded.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, err
	}
	if len(claims) > request.Limit {
		return nil, ErrInvalidClaim
	}
	frozen := make([]Claim, len(claims))
	previous := request.AfterTaskID
	for i, claim := range claims {
		if claim.Validate() != nil || claim.Scope.Canonical() != scope.Canonical() || claim.PlanID != request.PlanID || (!previous.IsNil() && claim.TaskID.String() <= previous.String()) {
			return nil, ErrInvalidClaim
		}
		frozen[i] = claim
		frozen[i].Scope.Levels = append([]cortex.Level(nil), claim.Scope.Levels...)
		previous = claim.TaskID
	}
	return frozen, nil
}
func (s *providerRecoveryScheduler) recoverRun(ctx context.Context, claim Claim) error {
	bounded, cancel := context.WithTimeout(ctx, s.config.PageTimeout)
	sealed, err := s.storage.GetRunCallSeal(bounded, claim.RunID)
	if err == nil {
		err = bounded.Err()
	}
	cancel()
	if err != nil {
		return err
	}
	if sealed == nil {
		return nil
	}
	if sealed.Validate() != nil || sealed.Request.RunID != claim.RunID || sealed.Request.Scope.Canonical() != claim.Scope.Canonical() {
		return execution.ErrRunCallConflict
	}
	seal := *sealed
	seal.Request.Scope.Levels = append([]cortex.Level(nil), sealed.Request.Scope.Levels...)
	request := execution.ProviderRecoveryPageRequest{Seal: seal, Limit: s.config.PageSize}
	for {
		page, scanErr := s.reconciler.ReconcilePage(ctx, request)
		if scanErr != nil {
			return scanErr
		}
		if page.NextAfter == "" {
			return ctx.Err()
		}
		request.After = page.NextAfter
		if waitErr := waitRecoveryPage(ctx, s.config.Interval); waitErr != nil {
			return waitErr
		}
	}
}

// The enclosing service cancels the shared lifetime and joins every Tick.
func (*providerRecoveryScheduler) Shutdown(context.Context) error { return nil }
