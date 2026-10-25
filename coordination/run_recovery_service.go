package coordination

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

type recoveringRunStore interface {
	execution.RunCallSealStore
	ListTaskClaims(context.Context, ClaimListRequest) ([]Claim, error)
}

type runRecoveryConfig struct {
	ClaimState                              ClaimState
	AcceptedClaims                          AcceptedClaimStore
	Discovery                               TaskPlanSource
	Interval, DiscoveryTimeout, PageTimeout time.Duration
	PageSize, MaxInFlight, MaxPerScope      int
	ReportError                             func(error)
	SealMissing                             func(context.Context, Claim) (execution.RunCallSeal, error)
}

type runRecoveryPage func(context.Context, Claim, execution.RunCallSeal, string, int) (string, error)

func newRunRecoveryService(config runRecoveryConfig, storage recoveringRunStore, makeScan func() (runRecoveryPage, error)) (*TaskService, error) {
	if config.ClaimState == "" {
		config.ClaimState = ClaimRecovering
	}
	if (config.ClaimState != ClaimRecovering && config.ClaimState != ClaimCompleted) || (config.AcceptedClaims != nil && config.ClaimState != ClaimCompleted) {
		return nil, ErrInvalidScheduler
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
	return newPlanService(TaskServiceConfig{Discovery: config.Discovery, Interval: config.Interval, DiscoveryTimeout: config.DiscoveryTimeout, MaxInFlight: config.MaxInFlight, ReportError: config.ReportError, Scheduler: SchedulerConfig{MaxPerScope: config.MaxPerScope}}, func() (serviceScheduler, error) {
		scan, err := makeScan()
		if err != nil {
			return nil, err
		}
		return &runRecoveryScheduler{storage: storage, scan: scan, config: config}, nil
	})
}

type runRecoveryScheduler struct {
	storage recoveringRunStore
	scan    runRecoveryPage
	config  runRecoveryConfig
}

func (s *runRecoveryScheduler) Tick(ctx context.Context, planID id.ID) error {
	var after id.ID
	// Keep only one error for this failed pass. A run-specific denial or outage
	// must not starve later claims or accumulate unbounded error data.
	var firstFailure error
	for {
		claims, err := s.claimPage(ctx, planID, after)
		if err != nil {
			return err
		}
		for _, claim := range claims {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			if claim.State == s.config.ClaimState {
				if recoveryErr := s.recoverRun(ctx, claim); recoveryErr != nil {
					if firstFailure == nil {
						firstFailure = recoveryErr
					}
				}
			}
		}
		if len(claims) < s.config.PageSize {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			return firstFailure
		}
		after = s.claimPageKey(claims[len(claims)-1])
		if waitErr := waitRecoveryPage(ctx, s.config.Interval); waitErr != nil {
			return waitErr
		}
	}
}
func (s *runRecoveryScheduler) claimPage(ctx context.Context, planID, after id.ID) ([]Claim, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	bounded, cancel := context.WithTimeout(ctx, s.config.PageTimeout)
	defer cancel()
	var claims []Claim
	if s.config.AcceptedClaims != nil {
		request := TaskAcceptanceListRequest{PlanID: planID, After: after, Limit: s.config.PageSize}
		if requestErr := request.Validate(); requestErr != nil {
			return nil, requestErr
		}
		claims, err = s.config.AcceptedClaims.ListAcceptedTaskClaims(bounded, request)
	} else {
		request := ClaimListRequest{PlanID: planID, AfterTaskID: after, Limit: s.config.PageSize}
		if requestErr := request.Validate(); requestErr != nil {
			return nil, requestErr
		}
		claims, err = s.storage.ListTaskClaims(bounded, request)
	}
	if contextErr := bounded.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, err
	}
	if len(claims) > s.config.PageSize {
		return nil, ErrInvalidClaim
	}
	frozen := make([]Claim, len(claims))
	previous := after
	for i, claim := range claims {
		key := s.claimPageKey(claim)
		if s.config.AcceptedClaims != nil && claim.State != ClaimCompleted {
			return nil, ErrInvalidClaim
		}
		if claim.Validate() != nil || key.IsNil() || claim.Scope.Canonical() != scope.Canonical() || claim.PlanID != planID || (!previous.IsNil() && key.String() <= previous.String()) {
			return nil, ErrInvalidClaim
		}
		frozen[i] = claim
		frozen[i].Scope.Levels = append([]cortex.Level(nil), claim.Scope.Levels...)
		if claim.AcceptanceID != nil {
			accepted := *claim.AcceptanceID
			frozen[i].AcceptanceID = &accepted
		}
		previous = key
	}
	return frozen, nil
}
func (s *runRecoveryScheduler) claimPageKey(claim Claim) id.ID {
	if s.config.AcceptedClaims != nil {
		if claim.AcceptanceID == nil {
			return id.ID{}
		}
		return *claim.AcceptanceID
	}
	return claim.TaskID
}

func (s *runRecoveryScheduler) recoverRun(ctx context.Context, claim Claim) error {
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
		if s.config.SealMissing == nil {
			return nil
		}
		created, sealErr := s.config.SealMissing(ctx, claim)
		if sealErr != nil {
			return sealErr
		}
		sealed = &created
	}
	if sealed.Validate() != nil || sealed.Request.RunID != claim.RunID || sealed.Request.Scope.Canonical() != claim.Scope.Canonical() {
		return execution.ErrRunCallConflict
	}
	seal := *sealed
	seal.Request.Scope.Levels = append([]cortex.Level(nil), sealed.Request.Scope.Levels...)
	after := ""
	for {
		next, scanErr := s.scan(ctx, claim, seal, after, s.config.PageSize)
		if scanErr != nil {
			return scanErr
		}
		if next == "" {
			return ctx.Err()
		}
		after = next
		if waitErr := waitRecoveryPage(ctx, s.config.Interval); waitErr != nil {
			return waitErr
		}
	}
}

// The enclosing service cancels the shared lifetime and joins every Tick.
func (*runRecoveryScheduler) Shutdown(context.Context) error { return nil }

// authorizeRunScan detaches metadata before a host callback and checks its lifetime.
func authorizeRunScan(ctx context.Context, authorize func(context.Context, Claim, execution.RunCallSeal) error, claim Claim, seal execution.RunCallSeal) error {
	frozenClaim, err := sealClaim(claim)
	if err != nil {
		return err
	}
	frozenRequest, _, err := seal.Request.Freeze()
	if err != nil {
		return err
	}
	seal.Request = frozenRequest
	if err := authorize(ctx, frozenClaim, seal); err != nil {
		return err
	}
	return ctx.Err()
}
