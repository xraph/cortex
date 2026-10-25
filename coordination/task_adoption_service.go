package coordination

import (
	"context"
	"time"

	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/internal/hostport"
)

// TaskAdoptionStore supports scoped discovery, sealed snapshots and atomic
// adoption. All journal writers must honor the same admission guard.
type TaskAdoptionStore interface {
	TaskRecoveryStore
	execution.RunRecoverySnapshotStore
	recoveringRunStore
}

// TaskRecoveryResolver obtains current task/result/prerequisite bindings and
// retained quiescence proof for the original claim and snapshot. Return nil when
// proof or verified output is not ready. It must honor cancellation, never invoke
// the task, and reuse the exact request ID and contents after uncertain commits.
// Resolution grants no authority: the recovery fence and authorizer run afterward.
type TaskRecoveryResolver interface {
	ResolveTaskRecovery(context.Context, Claim, execution.RunRecoverySnapshot) (*TaskAcceptanceRequest, error)
}

// TaskAdoptionServiceConfig installs optional adoption of sealed recovering runs.
// Discovery authorizes plan/claim reads; AuthorizeScan authorizes each snapshot
// before storage or resolution. Fence and Authorizer implement the complete
// recovery proof contract. AttemptTimeout includes scan, resolution and adoption.
// Concrete callbacks must honor cancellation and retain original dispatch identity.
type TaskAdoptionServiceConfig struct {
	Store            TaskAdoptionStore
	Discovery        TaskPlanSource
	AuthorizeScan    func(context.Context, Claim, execution.RunCallSeal) error
	Resolver         TaskRecoveryResolver
	Fence            TaskRecoveryFence
	Authorizer       TaskRecoveryAuthorizer
	AttemptTimeout   time.Duration
	Interval         time.Duration
	DiscoveryTimeout time.Duration
	PageTimeout      time.Duration
	PageSize         int
	MaxInFlight      int
	MaxPerScope      int
	ReportError      func(error)
}

// TaskAdoptionService owns scoped discovery and guarded adoption through shutdown.
// It never seals journals, invokes tasks or settles reservations. Failed or pending
// recovery is revisited on later discovery passes; committed receipts remain durable.
type TaskAdoptionService struct{ service *TaskService }

// NewTaskAdoptionService validates configuration without starting work. Defaults
// match provider recovery: four plan passes, one per scope, 64 claims per page,
// five seconds per claim/seal read and one second between pages. Each full
// adoption attempt defaults to thirty seconds and cannot exceed that duration.
func NewTaskAdoptionService(config TaskAdoptionServiceConfig) (*TaskAdoptionService, error) {
	if hostport.IsNil(config.Store) || hostport.IsNil(config.Resolver) || config.AuthorizeScan == nil {
		return nil, ErrInvalidScheduler
	}
	if config.AttemptTimeout == 0 {
		config.AttemptTimeout = 30 * time.Second
	}
	if config.AttemptTimeout < time.Millisecond || config.AttemptTimeout > 30*time.Second {
		return nil, ErrInvalidScheduler
	}
	adopter, err := NewTaskRecoveryAdopter(config.Store, config.Fence, config.Authorizer)
	if err != nil {
		return nil, err
	}
	service, err := newRunRecoveryService(runRecoveryConfig{Discovery: config.Discovery, Interval: config.Interval, DiscoveryTimeout: config.DiscoveryTimeout, PageTimeout: config.PageTimeout, PageSize: config.PageSize, MaxInFlight: config.MaxInFlight, MaxPerScope: config.MaxPerScope, ReportError: config.ReportError}, config.Store, func() (runRecoveryPage, error) {
		return func(ctx context.Context, claim Claim, seal execution.RunCallSeal, _ string, _ int) (string, error) {
			return "", adoptRecoveredRun(ctx, config, adopter, claim, seal)
		}, nil
	})
	if err != nil {
		return nil, err
	}
	return &TaskAdoptionService{service: service}, nil
}

func adoptRecoveredRun(ctx context.Context, config TaskAdoptionServiceConfig, adopter TaskRecoveryAdopter, claim Claim, seal execution.RunCallSeal) error {
	bounded, cancel := context.WithTimeout(ctx, config.AttemptTimeout)
	defer cancel()
	// Host callbacks receive detached metadata, never the scheduler's retained copy.
	scanClaim, err := sealClaim(claim)
	if err != nil {
		return err
	}
	scanRequest, _, err := seal.Request.Freeze()
	if err != nil {
		return err
	}
	scanSeal := seal
	scanSeal.Request = scanRequest
	if authorizeErr := config.AuthorizeScan(bounded, scanClaim, scanSeal); authorizeErr != nil {
		return authorizeErr
	}
	if contextErr := bounded.Err(); contextErr != nil {
		return contextErr
	}
	snapshot, err := config.Store.SnapshotRunRecovery(bounded, seal)
	if err != nil {
		return err
	}
	if contextErr := bounded.Err(); contextErr != nil {
		return contextErr
	}
	if snapshot.Validate() != nil || snapshot.Seal.RequestDigest != seal.RequestDigest || !snapshot.Seal.At.Equal(seal.At) {
		return ErrTaskAcceptanceConflict
	}
	frozen, err := execution.SealRunRecoverySnapshot(snapshot)
	if err != nil {
		return err
	}
	resolverClaim, err := sealClaim(claim)
	if err != nil {
		return err
	}
	request, err := config.Resolver.ResolveTaskRecovery(bounded, resolverClaim, frozen)
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
	if req.Claim != claim.Token() || req.Result.RunID != claim.RunID || req.Current.Scope.Canonical() != claim.Scope.Canonical() || req.Recovery == nil || req.Recovery.ClaimDigest != claim.Digest || req.Recovery.SnapshotDigest != snapshot.Digest || req.Recovery.Seal.RequestDigest != seal.RequestDigest || !req.Recovery.Seal.At.Equal(seal.At) {
		return ErrTaskAcceptanceConflict
	}
	_, err = adopter.Adopt(bounded, req)
	return err
}

// Start preserves service identity values and owns an independent lifetime.
func (s *TaskAdoptionService) Start(ctx context.Context) error { return s.service.Start(ctx) }

// RequestStop signals cancellation without claiming drainage.
func (s *TaskAdoptionService) RequestStop() { s.service.RequestStop() }

// Stop joins discovery, snapshot reads, resolution and adoption callbacks.
func (s *TaskAdoptionService) Stop(ctx context.Context) error { return s.service.Stop(ctx) }

// Status reports local counters without task, proof or result content.
func (s *TaskAdoptionService) Status() TaskServiceStatus { return s.service.Status() }
