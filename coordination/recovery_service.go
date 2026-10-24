package coordination

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

// ClaimRecoveryConfig connects authorized discovery to scoped claim expiry.
// Discovery must include plans with outstanding claims even after cancellation
// or completion. PageTimeout bounds one storage page, not an entire plan pass.
// MaxInFlight bounds concurrent plan passes; MaxPerScope bounds each exact scope.
// ReportError must return promptly. Store callbacks must honor cancellation.
type ClaimRecoveryConfig struct {
	Store            ClaimExpiryStore
	Clock            verification.Clock
	Discovery        TaskPlanSource
	Interval         time.Duration
	DiscoveryTimeout time.Duration
	PageTimeout      time.Duration
	PageSize         int
	MaxInFlight      int
	MaxPerScope      int
	ReportError      func(error)
}

// ClaimRecoveryService owns bounded discovery and paged expiry until Stop drains
// it. It does not execute agents, reconcile providers, accept results or refund
// reservations. Restart rereads from the beginning, preserving durable outcomes.
type ClaimRecoveryService struct{ service *TaskService }

// NewClaimRecoveryService validates explicit ports without starting work. Defaults
// are four concurrent plans, one per scope, 256 claims per page, a five-second page
// timeout and a one-second interval between pages. A pass keeps its slot until it
// reaches the end or fails. The host source must eventually finish each pass.
func NewClaimRecoveryService(config ClaimRecoveryConfig) (*ClaimRecoveryService, error) {
	sweeper, err := NewClaimSweeper(config.Store, config.Clock)
	if err != nil {
		return nil, err
	}
	if config.PageSize == 0 {
		config.PageSize = 256
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
		return &claimRecoveryScheduler{sweeper: sweeper, pageSize: config.PageSize, pageTimeout: config.PageTimeout, interval: config.Interval}, nil
	})
	if err != nil {
		return nil, err
	}
	return &ClaimRecoveryService{service: service}, nil
}

// Start preserves service identity values and owns an independent lifetime.
func (s *ClaimRecoveryService) Start(ctx context.Context) error { return s.service.Start(ctx) }

// RequestStop signals cancellation. Stop must still join the owned work.
func (s *ClaimRecoveryService) RequestStop() { s.service.RequestStop() }

// Stop waits for discovery and all page callbacks before returning success.
func (s *ClaimRecoveryService) Stop(ctx context.Context) error { return s.service.Stop(ctx) }

// Status reports local process counters without scope or run content.
func (s *ClaimRecoveryService) Status() TaskServiceStatus { return s.service.Status() }

type claimRecoveryScheduler struct {
	sweeper               *ClaimSweeper
	pageSize              int
	pageTimeout, interval time.Duration
}

func (s *claimRecoveryScheduler) Tick(ctx context.Context, planID id.ID) error {
	request := ClaimListRequest{PlanID: planID, Limit: s.pageSize}
	for {
		pageCtx, cancel := context.WithTimeout(ctx, s.pageTimeout)
		page, err := s.sweeper.Sweep(pageCtx, request)
		if err == nil {
			err = pageCtx.Err()
		}
		cancel()
		if err != nil {
			return err
		}
		if page.NextTaskID.IsNil() {
			return nil
		}
		request.AfterTaskID = page.NextTaskID
		if waitErr := waitRecoveryPage(ctx, s.interval); waitErr != nil {
			return waitErr
		}
	}
}

// The enclosing service cancels the shared lifetime and joins every Tick.
func (*claimRecoveryScheduler) Shutdown(context.Context) error { return nil }

// waitRecoveryPage throttles page traversal while retaining cancellation.
func waitRecoveryPage(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
