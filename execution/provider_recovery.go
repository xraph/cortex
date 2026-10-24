package execution

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/internal/hostport"
)

// ErrProviderCallUncertain keeps unresolved provider calls reserved.
var ErrProviderCallUncertain = cortex.ErrProviderCallUncertain

// ProviderCallRecoveryAdapter queries retained provider state for one original
// call. It must never generate, execute, resend or change the provider operation.
// A lookup miss, partial stream or inconclusive proof returns uncertainty.
// Implementations must honor cancellation and retain original routing evidence.
type ProviderCallRecoveryAdapter interface {
	ReconcileProviderCall(context.Context, RunCallRequest) (ProviderCallReceipt, error)
}

// ProviderCallRecoveryFence holds current host authority through the synchronous
// commit callback. Its guard must cover revocation and relevant dispatch routing
// revisions. It does not renew expired execution leases or authorize new calls.
type ProviderCallRecoveryFence interface {
	WithProviderCallFence(context.Context, RunCallRequest, func() error) error
}

// ProviderRecoveryConfig supplies mandatory trusted host ports. Authorize checks
// the current caller before lookup and inside the commit fence. Authenticate
// verifies terminal proof, original provider/account/routing binding and usage,
// including when returning stored proof. No default authorizer or verifier exists.
// Timeout includes waiting for a slot; adapters, stores and guards must honor it.
type ProviderRecoveryConfig struct {
	Store        ProviderCallReceiptStore
	Adapter      ProviderCallRecoveryAdapter
	Authorize    func(context.Context, RunCallRequest) error
	Authenticate func(context.Context, RunCallRequest, ProviderCallReceipt) error
	Fence        ProviderCallRecoveryFence
	// AuthorizeScan is required only by ReconcilePage. It checks current read
	// and recovery permission for the exact sealed run before and after a page.
	AuthorizeScan func(context.Context, RunCallSeal) error
	Timeout       time.Duration
	MaxConcurrent int
}

// ProviderReconciler authenticates and retains terminal proof for journaled model
// calls. It exposes no execution path. Callers discover work through bounded
// RunCallStore pages and authorize any later adoption or settlement separately.
type ProviderReconciler struct {
	config    ProviderRecoveryConfig
	slots     chan struct{}
	scanSlots chan struct{}
}

// NewProviderReconciler defaults to four concurrent operations and thirty seconds
// per operation, including queue wait. The timeout cannot exceed thirty seconds.
func NewProviderReconciler(config ProviderRecoveryConfig) (*ProviderReconciler, error) {
	if config.Timeout == 0 {
		config.Timeout = 30 * time.Second
	}
	if config.MaxConcurrent == 0 {
		config.MaxConcurrent = 4
	}
	if hostport.IsNil(config.Store) || hostport.IsNil(config.Adapter) || config.Authorize == nil || config.Authenticate == nil || hostport.IsNil(config.Fence) || config.Timeout < time.Millisecond || config.Timeout > 30*time.Second || config.MaxConcurrent < 1 || config.MaxConcurrent > 4096 {
		return nil, ErrInvalidRunCall
	}
	return &ProviderReconciler{config: config, slots: make(chan struct{}, config.MaxConcurrent), scanSlots: make(chan struct{}, config.MaxConcurrent)}, nil
}

// Reconcile queries and authenticates a single original call, or reauthenticates
// its immutable receipt. Lease expiry never proves an outcome. Errors can follow
// a durable write; repeating reconciliation reads proof and cannot dispatch.
func (p *ProviderReconciler) Reconcile(ctx context.Context, request RunCallRequest) (ProviderCallReceipt, error) {
	if err := ctx.Err(); err != nil {
		return ProviderCallReceipt{}, err
	}
	r, digest, err := request.Freeze()
	if err != nil {
		return ProviderCallReceipt{}, err
	}
	if r.Kind != RunCallComplete && r.Kind != RunCallStream {
		return ProviderCallReceipt{}, ErrInvalidRunCall
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return ProviderCallReceipt{}, err
	}
	if scope.Canonical() != r.Scope.Canonical() {
		return ProviderCallReceipt{}, cortex.ErrInvalidScope
	}
	bounded, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	case <-bounded.Done():
		return ProviderCallReceipt{}, bounded.Err()
	}
	if contextErr := bounded.Err(); contextErr != nil {
		return ProviderCallReceipt{}, contextErr
	}
	if authErr := p.config.Authorize(bounded, cloneProviderRequest(r)); authErr != nil {
		return ProviderCallReceipt{}, authErr
	}
	if _, err = p.original(bounded, r, digest); err != nil {
		return ProviderCallReceipt{}, err
	}
	stored, err := p.config.Store.GetProviderCallReceipt(bounded, cloneProviderRequest(r))
	if err != nil {
		return ProviderCallReceipt{}, err
	}
	var receipt ProviderCallReceipt
	if stored != nil {
		receipt = *stored
	} else {
		if contextErr := bounded.Err(); contextErr != nil {
			return ProviderCallReceipt{}, contextErr
		}
		receipt, err = p.config.Adapter.ReconcileProviderCall(bounded, cloneProviderRequest(r))
		if contextErr := bounded.Err(); contextErr != nil {
			return ProviderCallReceipt{}, contextErr
		}
		if err != nil {
			return ProviderCallReceipt{}, errors.Join(ErrProviderCallUncertain, err)
		}
	}
	frozen, _, err := receipt.Freeze(r)
	if err != nil {
		return ProviderCallReceipt{}, err
	}
	return hostport.WithSynchronousFence(bounded, ErrInvalidRunCall, func(attemptCtx context.Context, next func(struct{}) error) error {
		return p.config.Fence.WithProviderCallFence(attemptCtx, cloneProviderRequest(r), func() error { return next(struct{}{}) })
	}, func(attemptCtx context.Context, _ struct{}) (ProviderCallReceipt, error) {
		if contextErr := attemptCtx.Err(); contextErr != nil {
			return ProviderCallReceipt{}, contextErr
		}
		if authErr := p.config.Authorize(attemptCtx, cloneProviderRequest(r)); authErr != nil {
			return ProviderCallReceipt{}, authErr
		}
		if proofErr := p.config.Authenticate(attemptCtx, cloneProviderRequest(r), frozen); proofErr != nil {
			return ProviderCallReceipt{}, proofErr
		}
		if checkErr := p.observe(attemptCtx, r, digest, frozen); checkErr != nil {
			return ProviderCallReceipt{}, checkErr
		}
		if stored == nil {
			if putErr := p.config.Store.PutProviderCallReceipt(attemptCtx, cloneProviderRequest(r), frozen); putErr != nil {
				return ProviderCallReceipt{}, putErr
			}
		}
		if checkErr := p.observe(attemptCtx, r, digest, frozen); checkErr != nil {
			return ProviderCallReceipt{}, checkErr
		}
		return frozen, nil
	})
}

func (p *ProviderReconciler) original(ctx context.Context, r RunCallRequest, digest string) (RunCallRecord, error) {
	if err := ctx.Err(); err != nil {
		return RunCallRecord{}, err
	}
	original, err := p.config.Store.GetRunCall(ctx, r.RunID, r.OperationDigest)
	if err != nil {
		return RunCallRecord{}, err
	}
	if original.Validate() != nil {
		return RunCallRecord{}, ErrInvalidRunCall
	}
	_, originalDigest, err := original.Request.Freeze()
	if err != nil || originalDigest != digest {
		return RunCallRecord{}, ErrRunCallConflict
	}
	return original, ctx.Err()
}

func (p *ProviderReconciler) observe(ctx context.Context, r RunCallRequest, digest string, receipt ProviderCallReceipt) error {
	original, err := p.original(ctx, r, digest)
	if err != nil {
		return err
	}
	return receipt.ValidateObservation(original)
}

func cloneProviderRequest(r RunCallRequest) RunCallRequest {
	r.Scope.Levels = append([]cortex.Level(nil), r.Scope.Levels...)
	return r
}
