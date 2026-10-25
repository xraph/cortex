package coordination

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
)

// TaskSettlementStore supports scoped discovery and atomic guarded accounting.
// Journal, verifier and budget writers must share the documented storage guards.
type TaskSettlementStore interface {
	TaskUsageSettlementStore
	TaskUsageClosureStore
	TaskUsageSnapshotStore
	execution.RunRecoverySnapshotStore
	recoveringRunStore
	GetBudgetedTaskClaim(context.Context, id.ID) (BudgetedClaim, error)
}

// TaskUsageSettlementResolver resolves retained billing proof for the exact
// observation and original allocation. Return nil while proof is pending. It
// must honor cancellation and must never execute or retry the task. Resolution
// grants no authority; the settlement fence and authorizer run afterward.
type TaskUsageSettlementResolver interface {
	ResolveTaskUsageSettlement(context.Context, TaskUsageSnapshot, TaskUsageClosure, execution.Reservation) (*TaskUsageSettlementRequest, error)
}

// TaskSettlementServiceConfig connects accepted claims to guarded settlement.
// Discovery authorizes plan/claim reads; AuthorizeScan covers budget, usage and
// receipt reads before any proof resolution. ClosureActor names the host identity
// authorized to close verifier admission. Concrete ports must honor cancellation.
type TaskSettlementServiceConfig struct {
	Store             TaskSettlementStore
	Discovery         TaskPlanSource
	AuthorizeScan     func(context.Context, Claim, execution.RunCallSeal) error
	ClosureActor      string
	ClosureFence      TaskUsageClosureFence
	ClosureAuthorizer TaskUsageClosureAuthorizer
	// Configure both ports to seal ordinary accepted task journals automatically.
	// ClosureActor also identifies the caller for this separate authorization.
	CallSealFence      TaskCallSealFence
	CallSealAuthorizer TaskCallSealAuthorizer
	Resolver           TaskUsageSettlementResolver
	Fence              TaskUsageSettlementFence
	Authorizer         TaskUsageSettlementAuthorizer
	AttemptTimeout     time.Duration
	Interval           time.Duration
	DiscoveryTimeout   time.Duration
	PageTimeout        time.Duration
	PageSize           int
	MaxInFlight        int
	MaxPerScope        int
	ReportError        func(error)
}

// TaskSettlementService owns discovery, admission closure and settlement until
// Stop drains every callback. Journal sealing needs its separate optional ports.
// The service never invokes task work.
type TaskSettlementService struct{ service *TaskService }

// NewTaskSettlementService validates explicit ports without starting work.
// Defaults are four plan passes, one per scope, 64 claims per page, five seconds
// per claim/seal read and one second between pages. Each full attempt defaults to
// thirty seconds and cannot exceed it. Unbudgeted claims and pending proof wait
// without refunds. Discovery must include completed or cancelled plans with debt.
func NewTaskSettlementService(config TaskSettlementServiceConfig) (*TaskSettlementService, error) {
	if hostport.IsNil(config.Store) || hostport.IsNil(config.Resolver) || config.AuthorizeScan == nil || !handoffText(config.ClosureActor, 512) {
		return nil, ErrInvalidScheduler
	}
	if config.AttemptTimeout == 0 {
		config.AttemptTimeout = 30 * time.Second
	}
	if config.AttemptTimeout < time.Millisecond || config.AttemptTimeout > 30*time.Second {
		return nil, ErrInvalidScheduler
	}
	closer, err := NewTaskUsageCloser(config.Store, config.ClosureFence, config.ClosureAuthorizer)
	if err != nil {
		return nil, err
	}
	settler, err := NewTaskUsageSettler(config.Store, config.Fence, config.Authorizer)
	if err != nil {
		return nil, err
	}
	traversal := runRecoveryConfig{ClaimState: ClaimCompleted, Discovery: config.Discovery, Interval: config.Interval, DiscoveryTimeout: config.DiscoveryTimeout, PageTimeout: config.PageTimeout, PageSize: config.PageSize, MaxInFlight: config.MaxInFlight, MaxPerScope: config.MaxPerScope, ReportError: config.ReportError}
	if config.CallSealFence != nil || config.CallSealAuthorizer != nil {
		storage, ok := config.Store.(TaskCallSealStore)
		if !ok {
			return nil, execution.ErrRunCallUnavailable
		}
		sealer, sealErr := NewTaskCallSealer(storage, config.CallSealFence, config.CallSealAuthorizer)
		if sealErr != nil {
			return nil, sealErr
		}
		traversal.SealMissing = func(ctx context.Context, claim Claim) (execution.RunCallSeal, error) {
			bounded, cancel := context.WithTimeout(ctx, config.AttemptTimeout)
			defer cancel()
			return sealer.Seal(bounded, TaskCallSealRequest{SchemaVersion: 1, Claim: claim.Token(), ClaimDigest: claim.Digest, Seal: execution.RunCallSealRequest{SchemaVersion: 1, Scope: claim.Scope, RunID: claim.RunID, Actor: config.ClosureActor, Reason: "close accepted task call admission"}})
		}
	}
	service, err := newRunRecoveryService(traversal, config.Store, func() (runRecoveryPage, error) {
		return func(ctx context.Context, claim Claim, seal execution.RunCallSeal, _ string, _ int) (string, error) {
			return "", settleCompletedRun(ctx, config, closer, settler, claim, seal)
		}, nil
	})
	if err != nil {
		return nil, err
	}
	return &TaskSettlementService{service: service}, nil
}

func settleCompletedRun(ctx context.Context, config TaskSettlementServiceConfig, closer TaskUsageCloser, settler TaskUsageSettler, claim Claim, seal execution.RunCallSeal) error {
	bounded, cancel := context.WithTimeout(ctx, config.AttemptTimeout)
	defer cancel()
	if err := authorizeRunScan(bounded, config.AuthorizeScan, claim, seal); err != nil {
		return err
	}
	retained, err := config.Store.GetTaskUsageSettlement(bounded, claim.RunID)
	if err != nil {
		return err
	}
	if retained != nil {
		if retained.Validate() != nil || retained.Request.Usage.Claim != claim.Token() || retained.Request.Usage.Seal.RequestDigest != seal.RequestDigest || !retained.Request.Usage.Seal.At.Equal(seal.At) {
			return ErrTaskSettlementConflict
		}
		_, err = settler.Settle(bounded, retained.Request)
		return err
	}
	acquisition, err := config.Store.GetBudgetedTaskClaim(bounded, claim.ID)
	if errors.Is(err, ErrClaimNotFound) {
		return bounded.Err()
	}
	if err != nil {
		return err
	}
	allocation, err := settlementAllocation(claim, acquisition)
	if err != nil {
		return err
	}
	snapshot, err := settlementSnapshot(bounded, config.Store, claim, seal)
	if err != nil {
		return err
	}
	summary, err := SummarizeTaskUsage(snapshot)
	if err != nil {
		return err
	}
	if summary.OpenJobs != 0 {
		return bounded.Err()
	}
	usage := TaskUsageRequest{Claim: claim.Token(), Seal: seal, SnapshotDigest: snapshot.Journal.Digest}
	closed, err := config.Store.GetTaskUsageClosure(bounded, claim.RunID)
	if err != nil {
		return err
	}
	if closed == nil {
		created, closeErr := closer.Close(bounded, TaskUsageClosureRequest{SchemaVersion: 1, Usage: usage, UsageDigest: snapshot.Digest, Actor: config.ClosureActor, Reason: "close accepted task usage for settlement"})
		if closeErr != nil {
			return closeErr
		}
		closed = &created
	}
	if closed.Validate() != nil || closed.Request.Usage.Claim != claim.Token() || closed.Request.Usage.Seal.RequestDigest != seal.RequestDigest || !closed.Request.Usage.Seal.At.Equal(seal.At) {
		return ErrTaskSettlementConflict
	}
	frozenClosure, err := SealTaskUsageClosure(*closed)
	if err != nil {
		return err
	}
	frozenClosure.Fresh = false
	// Keep the authoritative hashes before handing detached inputs to a host port.
	usageDigest, closureDigest := snapshot.Digest, closed.Digest
	reservationID, reservationDigest := allocation.Request.ID, allocation.RequestDigest
	if contextErr := bounded.Err(); contextErr != nil {
		return contextErr
	}
	request, err := config.Resolver.ResolveTaskUsageSettlement(bounded, snapshot, frozenClosure, allocation)
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
	if req.Usage.Claim != claim.Token() || req.Usage.Seal.RequestDigest != seal.RequestDigest || !req.Usage.Seal.At.Equal(seal.At) || req.Usage.SnapshotDigest != usage.SnapshotDigest || req.UsageDigest != usageDigest || req.ClosureDigest != closureDigest || req.ReservationID != reservationID || req.ReservationDigest != reservationDigest {
		return ErrTaskSettlementConflict
	}
	_, err = settler.Settle(bounded, req)
	return err
}

func settlementAllocation(claim Claim, acquired BudgetedClaim) (execution.Reservation, error) {
	c, r := acquired.Claim, acquired.Reservation
	if c.Validate() != nil || c.State != ClaimHeld || r.Validate() != nil || r.Settlement != nil || c.ID != claim.ID || c.PlanID != claim.PlanID || c.PlanDigest != claim.PlanDigest || c.PlanRevision != claim.PlanRevision || c.TaskID != claim.TaskID || c.Worker != claim.Worker || c.GrantRef != claim.GrantRef || c.Fence != claim.Fence || c.Scope.Canonical() != claim.Scope.Canonical() || r.Request.OwnerRef != claim.ID.String() || r.Request.Scope.Canonical() != claim.Scope.Canonical() || r.Request.PlanID != claim.PlanID || r.Request.PlanDigest != claim.PlanDigest || r.Request.TaskID != claim.TaskID || r.Request.Kind != execution.ReservationLeaf {
		return execution.Reservation{}, ErrTaskSettlementConflict
	}
	frozen, _, err := r.Request.Freeze()
	if err != nil {
		return execution.Reservation{}, err
	}
	r.Request = frozen
	return r, nil
}

func settlementSnapshot(ctx context.Context, store TaskSettlementStore, claim Claim, seal execution.RunCallSeal) (TaskUsageSnapshot, error) {
	journal, err := store.SnapshotRunRecovery(ctx, seal)
	if err != nil {
		return TaskUsageSnapshot{}, err
	}
	if journal.Validate() != nil || journal.Seal.RequestDigest != seal.RequestDigest || !journal.Seal.At.Equal(seal.At) {
		return TaskUsageSnapshot{}, ErrTaskSettlementConflict
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return TaskUsageSnapshot{}, contextErr
	}
	snapshot, err := store.SnapshotTaskUsage(ctx, TaskUsageRequest{Claim: claim.Token(), Seal: seal, SnapshotDigest: journal.Digest})
	if err != nil {
		return TaskUsageSnapshot{}, err
	}
	if snapshot.Validate() != nil || snapshot.Claim.Token() != claim.Token() || snapshot.Claim.Digest != claim.Digest || snapshot.Journal.Digest != journal.Digest {
		return TaskUsageSnapshot{}, ErrTaskSettlementConflict
	}
	return SealTaskUsageSnapshot(snapshot)
}

// Start preserves service identity values and owns an independent lifetime.
func (s *TaskSettlementService) Start(ctx context.Context) error { return s.service.Start(ctx) }

// RequestStop signals cancellation without claiming drainage.
func (s *TaskSettlementService) RequestStop() { s.service.RequestStop() }

// Stop joins discovery, storage, proof resolution and accounting callbacks.
func (s *TaskSettlementService) Stop(ctx context.Context) error { return s.service.Stop(ctx) }

// Status reports local counters without task content or billing references.
func (s *TaskSettlementService) Status() TaskServiceStatus { return s.service.Status() }
