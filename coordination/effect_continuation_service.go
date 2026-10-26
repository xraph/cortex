package coordination

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/internal/hostport"
)

// EffectContinuationOffer contains exact adapter arguments, never model authority.
// Return nil while an authenticated provider disposition or grant is unavailable.
type EffectContinuationOffer struct {
	Request   execution.EffectContinuationRequest
	Arguments []byte
}

// EffectContinuationResolver resolves proof and arguments without invoking tools.
// Inputs are detached and scope-authorized; the executor authenticates each offer
// again under current authority before reserving or dispatching an action.
type EffectContinuationResolver interface {
	ResolveEffectContinuation(context.Context, Claim, execution.RunRecoverySnapshot, execution.EffectRecord, []execution.EffectContinuation) (*EffectContinuationOffer, error)
}

// EffectContinuationServiceConfig enables explicit bounded follow-up actions.
// Effects.Store must implement continuation, discovery, recovery snapshots and
// recovering ownership. AuthorizeScan must permit private journal reads. Discovery
// covers plan/claim reads. All callbacks must honor cancellation until drain.
type EffectContinuationServiceConfig struct {
	Effects          execution.EffectExecutorConfig
	Authority        execution.EffectContinuationAuthority
	Resolver         EffectContinuationResolver
	Discovery        TaskPlanSource
	Interval         time.Duration
	DiscoveryTimeout time.Duration
	PageTimeout      time.Duration
	PageSize         int
	MaxInFlight      int
	MaxPerScope      int
	ReportError      func(error)
}

type effectContinuationServiceStore interface {
	recoveringRunStore
	execution.EffectContinuationStore
	execution.EffectDiscoveryStore
	execution.RunRecoverySnapshotStore
}

// EffectContinuationService owns discovery, provider reconciliation and admitted
// retries or compensation. It never dispatches replacement tasks or accepts old
// candidates. Its retained result evidence is input to a new verified preparation.
type EffectContinuationService struct{ service *TaskService }

// NewEffectContinuationService validates ports without starting work. It shares
// the existing bounded recovery traversal and resumes only from durable intent.
func NewEffectContinuationService(config EffectContinuationServiceConfig) (*EffectContinuationService, error) {
	store, ok := config.Effects.Store.(effectContinuationServiceStore)
	if !ok || hostport.IsNil(store) || hostport.IsNil(config.Authority) || hostport.IsNil(config.Resolver) || config.Effects.AuthorizeScan == nil {
		return nil, ErrInvalidScheduler
	}
	config.Effects.Adapters = append([]execution.EffectRegistration(nil), config.Effects.Adapters...)
	if _, err := execution.NewEffectExecutor(config.Effects); err != nil {
		return nil, err
	}
	if config.PageTimeout == 0 {
		config.PageTimeout = execution.MaxEffectDuration
	}
	if config.PageTimeout > execution.MaxEffectDuration {
		return nil, ErrInvalidScheduler
	}
	service, err := newRunRecoveryService(runRecoveryConfig{Discovery: config.Discovery, Interval: config.Interval, DiscoveryTimeout: config.DiscoveryTimeout, PageTimeout: config.PageTimeout, PageSize: config.PageSize, MaxInFlight: config.MaxInFlight, MaxPerScope: config.MaxPerScope, ReportError: config.ReportError}, store, func() (runRecoveryPage, error) {
		executor, makeErr := execution.NewEffectExecutor(config.Effects)
		if makeErr != nil {
			return nil, makeErr
		}
		return func(ctx context.Context, claim Claim, seal execution.RunCallSeal, after string, limit int) (string, error) {
			bounded, cancel := context.WithTimeout(ctx, config.PageTimeout)
			defer cancel()
			return continueEffectsPage(bounded, config, store, executor, claim, seal, after, limit)
		}, nil
	})
	if err != nil {
		return nil, err
	}
	return &EffectContinuationService{service: service}, nil
}
func continueEffectsPage(ctx context.Context, config EffectContinuationServiceConfig, store effectContinuationServiceStore, executor *execution.EffectExecutor, claim Claim, seal execution.RunCallSeal, after string, limit int) (string, error) {
	if err := authorizeContinuationScan(ctx, config, seal); err != nil {
		return "", err
	}
	effects, err := store.ListEffects(ctx, execution.EffectListRequest{RunID: claim.RunID, After: after, Limit: limit})
	if err != nil {
		return "", err
	}
	if len(effects) > limit {
		return "", execution.ErrEffectConflict
	}
	next := ""
	previous := after
	var firstFailure error
	for _, effect := range effects {
		if contextErr := ctx.Err(); contextErr != nil {
			return "", contextErr
		}
		if effect.Request.RunID != claim.RunID || effect.Request.Scope.Canonical() != claim.Scope.Canonical() {
			return "", execution.ErrEffectConflict
		}
		next = evidence.Digest([]byte(effect.Request.OperationKey))
		if next <= previous {
			return "", execution.ErrEffectConflict
		}
		previous = next
		if attemptErr := continueOneEffect(ctx, config, store, executor, claim, seal, effect); attemptErr != nil && firstFailure == nil {
			firstFailure = attemptErr
		}
	}
	if len(effects) < limit {
		next = ""
	}
	return next, firstFailure
}
func continueOneEffect(ctx context.Context, config EffectContinuationServiceConfig, store effectContinuationServiceStore, executor *execution.EffectExecutor, claim Claim, seal execution.RunCallSeal, effect execution.EffectRecord) error {
	if err := authorizeContinuationScan(ctx, config, seal); err != nil {
		return err
	}
	// Original and follow-up unknowns get observation-only recovery. Unknown
	// idempotent attempts may progress only with a new authenticated offer.
	if effect.Receipt == nil {
		_, err := executor.Reconcile(ctx, effect.Request)
		if err != nil && !errors.Is(err, execution.ErrEffectUncertain) {
			return err
		}
	}
	history, err := store.ListEffectContinuations(ctx, effect.Request)
	if err != nil {
		return err
	}
	for _, entry := range history {
		result, readErr := store.GetEffectContinuationResult(ctx, entry.Request)
		if readErr != nil {
			return readErr
		}
		if result == nil {
			_, readErr = executor.ReconcileContinuation(ctx, entry.Request, config.Authority)
			if readErr != nil && !errors.Is(readErr, execution.ErrEffectUncertain) {
				return readErr
			}
		}
	}
	snapshot, err := store.SnapshotRunRecovery(ctx, seal)
	if err != nil {
		return err
	}
	if snapshot.Validate() != nil {
		return execution.ErrInvalidEffect
	}
	current := execution.EffectRecord{}
	_, expected, err := effect.Request.Freeze()
	if err != nil {
		return err
	}
	for _, entry := range snapshot.Effects {
		_, digest, freezeErr := entry.Request.Freeze()
		if freezeErr == nil && digest == expected {
			current = entry
		}
	}
	if current.Request.RunCall == nil || current.Request.RunCall.ClaimID != claim.ID {
		return execution.ErrEffectConflict
	}
	offeredClaim, err := sealClaim(claim)
	if err != nil {
		return err
	}
	offeredSnapshot, err := execution.SealRunRecoverySnapshot(snapshot)
	if err != nil {
		return err
	}
	offeredOriginal, _, err := current.Request.Freeze()
	if err != nil {
		return err
	}
	offered := execution.EffectRecord{Request: offeredOriginal}
	if current.Receipt != nil {
		copyReceipt := *current.Receipt
		offered.Receipt = &copyReceipt
	}
	if scanErr := authorizeContinuationScan(ctx, config, seal); scanErr != nil {
		return scanErr
	}
	offer, err := config.Resolver.ResolveEffectContinuation(ctx, offeredClaim, offeredSnapshot, offered, history)
	if err != nil {
		return err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if offer == nil {
		return nil
	}
	request, _, err := offer.Request.Freeze()
	if err != nil {
		return err
	}
	_, digest, err := request.Original.Freeze()
	if err != nil {
		return err
	}
	if digest != expected || request.ClaimDigest != claim.Digest || request.SnapshotDigest != snapshot.Digest || request.Seal.RequestDigest != seal.RequestDigest || !request.Seal.At.Equal(seal.At) || request.Reservation.PlanID != claim.PlanID || request.Reservation.TaskID != claim.TaskID {
		return execution.ErrEffectConflict
	}
	_, err = executor.ContinueEffect(ctx, request, offer.Arguments, config.Authority)
	return err
}

// Start owns an independent service lifetime with the caller's identity values.
func (s *EffectContinuationService) Start(ctx context.Context) error { return s.service.Start(ctx) }

// RequestStop cancels callbacks without claiming they have drained.
func (s *EffectContinuationService) RequestStop() { s.service.RequestStop() }

// Stop joins discovery and every adapter callback before returning success.
func (s *EffectContinuationService) Stop(ctx context.Context) error { return s.service.Stop(ctx) }

// Status exposes local service counts without private provider content.
func (s *EffectContinuationService) Status() TaskServiceStatus { return s.service.Status() }

func authorizeContinuationScan(ctx context.Context, config EffectContinuationServiceConfig, seal execution.RunCallSeal) error {
	request, _, err := seal.Request.Freeze()
	if err != nil {
		return err
	}
	seal.Request = request
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := config.Effects.AuthorizeScan(ctx, seal); err != nil {
		return err
	}
	return ctx.Err()
}
