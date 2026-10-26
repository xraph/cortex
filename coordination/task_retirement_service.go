package coordination

import (
	"context"
	"time"

	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/internal/hostport"
)

// TaskRetirementServiceStore discovers recovering ownership and retains its
// closed accounting, sealed journal and retirement receipt.
type TaskRetirementServiceStore interface {
	TaskRetirementStore
	TaskUsageClosureStore
	TaskUsageSettlementStore
	execution.RunRecoverySnapshotStore
	recoveringRunStore
}

// TaskRetirementResolver obtains authenticated quiescence proof for the exact
// original claim, journal and accounting. Return nil while proof is pending.
// Honor cancellation and reuse request contents after an uncertain commit.
// Resolution must not invoke or retry work; the guarded retirer checks authority.
type TaskRetirementResolver interface {
	ResolveTaskRetirement(context.Context, Claim, execution.RunRecoverySnapshot, TaskUsageClosure, TaskUsageSettlement) (*TaskRetirementRequest, error)
}

// TaskRetirementServiceConfig connects authorized discovery to guarded retirement.
// Discovery covers plan/claim/seal reads. AuthorizeScan covers journal, closure,
// settlement and retirement reads before proof resolution. Callbacks must honor
// cancellation; the service retains ownership until every callback returns.
type TaskRetirementServiceConfig struct {
	Store            TaskRetirementServiceStore
	Discovery        TaskPlanSource
	AuthorizeScan    func(context.Context, Claim, execution.RunCallSeal) error
	Resolver         TaskRetirementResolver
	Fence            TaskRetirementFence
	Authorizer       TaskRetirementAuthorizer
	AttemptTimeout   time.Duration
	Interval         time.Duration
	DiscoveryTimeout time.Duration
	PageTimeout      time.Duration
	PageSize         int
	MaxInFlight      int
	MaxPerScope      int
	ReportError      func(error)
}

// TaskRetirementService owns discovery and retirement until shutdown drains.
// It never seals journals, settles budgets, dispatches replacements or accepts results.
type TaskRetirementService struct{ service *TaskService }

// NewTaskRetirementService validates explicit ports without starting work.
// Attempts default to thirty seconds, with the shared recovery traversal bounds.
// Discovery must retain plans with unsuccessful ownership, including expired plans.
func NewTaskRetirementService(config TaskRetirementServiceConfig) (*TaskRetirementService, error) {
	if hostport.IsNil(config.Store) || hostport.IsNil(config.Resolver) || config.AuthorizeScan == nil {
		return nil, ErrInvalidScheduler
	}
	if config.AttemptTimeout == 0 {
		config.AttemptTimeout = 30 * time.Second
	}
	if config.AttemptTimeout < time.Millisecond || config.AttemptTimeout > 30*time.Second {
		return nil, ErrInvalidScheduler
	}
	retirer, err := NewTaskRetirer(config.Store, config.Fence, config.Authorizer)
	if err != nil {
		return nil, err
	}
	service, err := newRunRecoveryService(runRecoveryConfig{Discovery: config.Discovery, Interval: config.Interval, DiscoveryTimeout: config.DiscoveryTimeout, PageTimeout: config.PageTimeout, PageSize: config.PageSize, MaxInFlight: config.MaxInFlight, MaxPerScope: config.MaxPerScope, ReportError: config.ReportError}, config.Store, func() (runRecoveryPage, error) {
		return func(ctx context.Context, claim Claim, seal execution.RunCallSeal, _ string, _ int) (string, error) {
			return "", retireTaskRun(ctx, config, retirer, claim, seal)
		}, nil
	})
	if err != nil {
		return nil, err
	}
	return &TaskRetirementService{service: service}, nil
}

func retireTaskRun(ctx context.Context, config TaskRetirementServiceConfig, retirer TaskRetirer, claim Claim, seal execution.RunCallSeal) error {
	bounded, cancel := context.WithTimeout(ctx, config.AttemptTimeout)
	defer cancel()
	if err := authorizeRunScan(bounded, config.AuthorizeScan, claim, seal); err != nil {
		return err
	}
	retained, err := config.Store.GetTaskRetirement(bounded, claim.ID)
	if err != nil {
		return err
	}
	if retained != nil {
		if retained.Validate() != nil || !retirementMatchesClaim(retained.Request, claim, seal) {
			return ErrClaimConflict
		}
		_, err = retirer.Retire(bounded, retained.Request)
		return err
	}
	settled, err := config.Store.GetTaskUsageSettlement(bounded, claim.RunID)
	if err != nil {
		return err
	}
	if settled == nil {
		return bounded.Err()
	}
	if settled.Validate() != nil || settled.Request.Usage.Claim != claim.Token() || settled.Request.Usage.Seal.RequestDigest != seal.RequestDigest || !settled.Request.Usage.Seal.At.Equal(seal.At) {
		return ErrClaimConflict
	}
	closed, err := config.Store.GetTaskUsageClosure(bounded, claim.RunID)
	if err != nil {
		return err
	}
	if closed == nil || closed.Validate() != nil || !closed.Request.MatchesClaim(claim) || (closed.Request.Termination == nil && closed.Request.Rejection == nil) || closed.Digest != settled.Request.ClosureDigest {
		return ErrClaimConflict
	}
	snapshot, err := config.Store.SnapshotRunRecovery(bounded, seal)
	if err != nil {
		return err
	}
	if snapshot.Validate() != nil || snapshot.Seal.RequestDigest != seal.RequestDigest || !snapshot.Seal.At.Equal(seal.At) || snapshot.Digest != settled.Request.Usage.SnapshotDigest {
		return ErrClaimConflict
	}
	if contextErr := bounded.Err(); contextErr != nil {
		return contextErr
	}
	// Each resolver input is detached from the bindings checked after it returns.
	offered, err := sealClaim(claim)
	if err != nil {
		return err
	}
	frozen, err := execution.SealRunRecoverySnapshot(snapshot)
	if err != nil {
		return err
	}
	frozenClosure, err := SealTaskUsageClosure(*closed)
	if err != nil {
		return err
	}
	frozenSettlement, err := SealTaskUsageSettlement(*settled)
	if err != nil {
		return err
	}
	request, err := config.Resolver.ResolveTaskRetirement(bounded, offered, frozen, frozenClosure, frozenSettlement)
	if err != nil {
		return err
	}
	if contextErr := bounded.Err(); contextErr != nil {
		return contextErr
	}
	if request == nil {
		return nil
	}
	req, _, err := request.Freeze()
	if err != nil {
		return err
	}
	if !retirementMatchesClaim(req, claim, seal) || req.Recovery.SnapshotDigest != snapshot.Digest || req.ClosureDigest != closed.Digest || req.SettlementDigest != settled.Digest {
		return ErrClaimConflict
	}
	_, err = retirer.Retire(bounded, req)
	return err
}

func retirementMatchesClaim(req TaskRetirementRequest, claim Claim, seal execution.RunCallSeal) bool {
	return req.Claim == claim.Token() && req.Recovery.ClaimDigest == claim.Digest && req.Recovery.Seal.RequestDigest == seal.RequestDigest && req.Recovery.Seal.At.Equal(seal.At) && req.Recovery.Seal.Request.Scope.Canonical() == claim.Scope.Canonical() && req.Recovery.Seal.Request.RunID == claim.RunID
}

// Start preserves service identity values and owns an independent lifetime.
func (s *TaskRetirementService) Start(ctx context.Context) error { return s.service.Start(ctx) }

// RequestStop signals cancellation without claiming drainage.
func (s *TaskRetirementService) RequestStop() { s.service.RequestStop() }

// Stop joins discovery, proof resolution and guarded retirement callbacks.
func (s *TaskRetirementService) Stop(ctx context.Context) error { return s.service.Stop(ctx) }

// Status reports local counters without proof or task content.
func (s *TaskRetirementService) Status() TaskServiceStatus { return s.service.Status() }
