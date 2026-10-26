package coordination

import (
	"context"
	"sync"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/planning"
)

// ArtifactPublicationStore discovers accepted output and retains board decisions.
type ArtifactPublicationStore interface {
	ArtifactBoardStore
	TaskAcceptanceStore
}

// ArtifactPublicationResolver supplies one currently authorized publication for
// the exact acceptance. Return nil while unresolved. It must not invoke tools or
// models. Conflict resolution is an explicit separate ArtifactBoard.Write action.
type ArtifactPublicationResolver interface {
	ResolveArtifactPublication(context.Context, TaskAcceptance) (*ArtifactBoardRequest, error)
}

// ArtifactPublicationConfig connects accepted output discovery to the guarded
// board. Only one offer per acceptance is processed per pass. Current source and
// evidence bindings must be resolved under the board authority fence.
type ArtifactPublicationConfig struct {
	Store            ArtifactPublicationStore
	Discovery        TaskPlanSource
	Authority        ArtifactBoardAuthority
	Resolver         ArtifactPublicationResolver
	AuthorizeScan    func(context.Context, id.ID) error
	PageSize         int
	PageTimeout      time.Duration
	Interval         time.Duration
	DiscoveryTimeout time.Duration
	MaxInFlight      int
	MaxPerScope      int
	ReportError      func(error)
}

// ArtifactPublicationService publishes qualified accepted outputs and retains
// conflicts for explicit resolution. It never edits accepted runs or their output.
type ArtifactPublicationService struct{ service *TaskService }
type artifactPublicationWorker struct {
	config  ArtifactPublicationConfig
	board   *ArtifactBoard
	mu      sync.Mutex
	cursors map[string]id.ID
}

// NewArtifactPublicationService validates ports and bounds without starting work.
func NewArtifactPublicationService(config ArtifactPublicationConfig) (*ArtifactPublicationService, error) {
	if hostport.IsNil(config.Store) || hostport.IsNil(config.Resolver) || config.AuthorizeScan == nil {
		return nil, ErrInvalidScheduler
	}
	if config.PageSize == 0 {
		config.PageSize = 16
	}
	if config.PageTimeout == 0 {
		config.PageTimeout = 30 * time.Second
	}
	if config.PageSize < 1 || config.PageSize > 256 || config.PageTimeout < time.Millisecond || config.PageTimeout > 30*time.Second {
		return nil, ErrInvalidScheduler
	}
	board, err := NewArtifactBoard(config.Store, config.Authority)
	if err != nil {
		return nil, err
	}
	worker := &artifactPublicationWorker{config: config, board: board, cursors: make(map[string]id.ID)}
	service, err := newIdentityOperation(planning.PrefixPlan, TaskServiceConfig{Discovery: config.Discovery, Interval: config.Interval, DiscoveryTimeout: config.DiscoveryTimeout, MaxInFlight: config.MaxInFlight, ReportError: config.ReportError, Scheduler: SchedulerConfig{MaxPerScope: config.MaxPerScope}}, config.PageTimeout, worker.tick)
	if err != nil {
		return nil, err
	}
	return &ArtifactPublicationService{service: service}, nil
}
func (w *artifactPublicationWorker) tick(ctx context.Context, planID id.ID) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	key := scope.Canonical() + "\x00" + planID.String()
	w.mu.Lock()
	after, exists := w.cursors[key]
	if !exists && len(w.cursors) >= 4096 {
		w.mu.Unlock()
		return ErrInvalidScheduler
	}
	w.cursors[key] = after
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		if after.IsNil() {
			delete(w.cursors, key)
		} else {
			w.cursors[key] = after
		}
		w.mu.Unlock()
	}()
	c := w.config
	if scanErr := c.AuthorizeScan(ctx, planID); scanErr != nil {
		return scanErr
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	accepted, err := c.Store.ListTaskAcceptances(ctx, TaskAcceptanceListRequest{PlanID: planID, After: after, Limit: c.PageSize})
	if err != nil {
		return err
	}
	if len(accepted) > c.PageSize {
		return planning.ErrInvalid
	}
	var firstError error
	for _, receipt := range accepted {
		if receipt.Validate() != nil || receipt.Claim.PlanID != planID || receipt.Claim.Scope.Canonical() != scope.Canonical() {
			return planning.ErrConflict
		}
		if publishErr := w.publish(ctx, receipt); publishErr != nil && firstError == nil {
			firstError = publishErr
		}
		after = receipt.Request.ID
	}
	if len(accepted) < c.PageSize {
		after = id.ID{}
	}
	return firstError
}
func (w *artifactPublicationWorker) publish(ctx context.Context, receipt TaskAcceptance) error {
	if err := w.config.AuthorizeScan(ctx, receipt.Claim.PlanID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	offered, err := SealTaskAcceptance(receipt)
	if err != nil {
		return err
	}
	request, err := w.config.Resolver.ResolveArtifactPublication(ctx, offered)
	if err != nil {
		return err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if request == nil {
		return nil
	}
	frozen, _, err := request.Freeze()
	if err != nil {
		return err
	}
	if !frozen.Resolve.IsNil() || frozen.PlanID != receipt.Claim.PlanID || frozen.PlanDigest != receipt.Claim.PlanDigest || frozen.AcceptanceID != receipt.Request.ID || frozen.AcceptanceDigest != receipt.Digest || frozen.Scope.Canonical() != receipt.Claim.Scope.Canonical() {
		return planning.ErrConflict
	}
	history, err := w.board.Read(ctx, ArtifactBoardRead{Scope: frozen.Scope, PlanID: frozen.PlanID, Resource: frozen.Resource, Limit: MaxBoardTransitions})
	if err != nil {
		return err
	}
	for _, entry := range history {
		if entry.Request.AcceptanceID == frozen.AcceptanceID && entry.Request.AcceptanceDigest == frozen.AcceptanceDigest {
			return nil
		}
	}
	_, err = w.board.Write(ctx, frozen)
	return err
}

// Start begins accepted-output discovery.
func (s *ArtifactPublicationService) Start(ctx context.Context) error { return s.service.Start(ctx) }

// RequestStop signals cancellation while retaining in-flight ownership.
func (s *ArtifactPublicationService) RequestStop() { s.service.RequestStop() }

// Stop waits for all publication callbacks to drain.
func (s *ArtifactPublicationService) Stop(ctx context.Context) error { return s.service.Stop(ctx) }

// Status exposes counters without private artifact metadata.
func (s *ArtifactPublicationService) Status() TaskServiceStatus { return s.service.Status() }
