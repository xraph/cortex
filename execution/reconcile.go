package execution

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
)

// EffectAction lets host policy distinguish mutations from recovery reads.
type EffectAction string

const (
	EffectActionExecute   EffectAction = "execute"
	EffectActionReconcile EffectAction = "reconcile"
)

// EffectRegistration pins trusted behavior to a handler revision.
type EffectRegistration struct {
	Requirement EffectRequirement
	Adapter     EffectAdapter
}

// EffectExecutorConfig has no permissive policy or in-memory storage fallback.
// Authorize authenticates the current caller, resolves live grants and checks
// run/assignment ownership. Adapters must enforce those grants at the write.
type EffectExecutorConfig struct {
	Store         EffectStore
	Adapters      []EffectRegistration
	Authorize     func(context.Context, EffectRequest, EffectAction) error
	MaxConcurrent int
}

// EffectExecutor dispatches an operation once and recovers through read-only
// reconciliation. It never automatically retries a mutation, including declared
// idempotent operations. Retrying a request returns its retained receipt or
// probes the original attempt; unknown outcomes remain reserved indefinitely.
type EffectExecutor struct {
	store     EffectStore
	adapters  map[string]EffectRegistration
	authorize func(context.Context, EffectRequest, EffectAction) error
	slots     chan struct{}
}

// NewEffectExecutor copies host registrations and rejects ambiguous revisions.
func NewEffectExecutor(cfg EffectExecutorConfig) (*EffectExecutor, error) {
	if effectNil(cfg.Store) || cfg.Authorize == nil || len(cfg.Adapters) < 1 || len(cfg.Adapters) > 256 || cfg.MaxConcurrent < 1 || cfg.MaxConcurrent > 64 {
		return nil, ErrEffectUnavailable
	}
	e := &EffectExecutor{store: cfg.Store, adapters: make(map[string]EffectRegistration, len(cfg.Adapters)), authorize: cfg.Authorize, slots: make(chan struct{}, cfg.MaxConcurrent)}
	for _, registration := range cfg.Adapters {
		if registration.Requirement.Validate() != nil || effectNil(registration.Adapter) {
			return nil, ErrInvalidEffect
		}
		key := registration.Requirement.Tool
		if _, exists := e.adapters[key]; exists {
			return nil, ErrEffectConflict
		}
		e.adapters[key] = registration
	}
	return e, nil
}
func effectNil(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Func, reflect.Slice, reflect.Chan:
		return r.IsNil()
	default:
		return false
	}
}

// Execute reserves before dispatch. An error after reservation is uncertain even
// if no adapter call occurred; recovery must inspect the original operation.
func (e *EffectExecutor) Execute(ctx context.Context, request EffectRequest, args []byte) (EffectReceipt, error) {
	if len(args) > 64<<10 || evidence.Digest(args) != request.ArgsDigest {
		return EffectReceipt{}, ErrInvalidEffect
	}
	data := append([]byte(nil), args...)
	return e.perform(ctx, request, data, false)
}

// Reconcile recovers an existing operation after a restart or deadline expiry.
// Fresh host authorization is required. It never creates or dispatches an effect.
func (e *EffectExecutor) Reconcile(ctx context.Context, request EffectRequest) (EffectReceipt, error) {
	return e.perform(ctx, request, nil, true)
}

func (e *EffectExecutor) perform(ctx context.Context, request EffectRequest, args []byte, recoverOnly bool) (EffectReceipt, error) {
	if err := ctx.Err(); err != nil {
		return EffectReceipt{}, err
	}
	r, digest, err := request.Freeze()
	if err != nil {
		return EffectReceipt{}, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return EffectReceipt{}, err
	}
	if scope.Canonical() != r.Scope.Canonical() {
		return EffectReceipt{}, cortex.ErrInvalidScope
	}
	registration, ok := e.adapters[r.Requirement.Tool]
	if !ok || registration.Requirement != r.Requirement {
		return EffectReceipt{}, ErrInvalidEffect
	}
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	default:
		return EffectReceipt{}, ErrEffectUnavailable
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	action := EffectActionReconcile
	if !recoverOnly {
		action = EffectActionExecute
		if !time.Now().Before(r.Deadline) {
			return EffectReceipt{}, context.DeadlineExceeded
		}
		var finish context.CancelFunc
		bounded, finish = context.WithDeadline(bounded, r.Deadline)
		defer finish()
	}
	if authErr := e.authorizeRequest(bounded, r, action); authErr != nil {
		return EffectReceipt{}, authErr
	}
	created := false
	if !recoverOnly {
		created, err = e.store.BeginEffect(bounded, cloneEffect(r))
		if err != nil {
			if errors.Is(err, ErrEffectConflict) {
				return EffectReceipt{}, err
			}
			return EffectReceipt{}, errors.Join(ErrEffectUncertain, err)
		}
	}
	if !created {
		recorded, readErr := e.store.GetEffect(bounded, r.OperationKey)
		if readErr != nil {
			return EffectReceipt{}, readErr
		}
		_, storedDigest, checkErr := recorded.Request.Freeze()
		if checkErr != nil || storedDigest != digest {
			return EffectReceipt{}, ErrEffectConflict
		}
		if recorded.Receipt != nil {
			receipt := *recorded.Receipt
			if receipt.Validate(r) != nil {
				return EffectReceipt{}, ErrInvalidEffect
			}
			if authErr := e.authorizeRequest(bounded, r, EffectActionReconcile); authErr != nil {
				return EffectReceipt{}, authErr
			}
			return receipt, nil
		}
		if authErr := e.authorizeRequest(bounded, r, EffectActionReconcile); authErr != nil {
			return EffectReceipt{}, authErr
		}
		status, probeErr := registration.Adapter.Reconcile(bounded, cloneEffect(r))
		if probeErr != nil || status.Receipt == nil || status.Outcome != status.Receipt.Outcome || status.Receipt.Validate(r) != nil {
			return EffectReceipt{}, ErrEffectUncertain
		}
		return e.complete(bounded, r, *status.Receipt)
	}
	if authErr := e.authorizeRequest(bounded, r, EffectActionExecute); authErr != nil {
		return EffectReceipt{}, ErrEffectUncertain
	}
	receipt, callErr := registration.Adapter.Execute(bounded, cloneEffect(r), args)
	if callErr != nil || receipt.Validate(r) != nil {
		return EffectReceipt{}, ErrEffectUncertain
	}
	return e.complete(bounded, r, receipt)
}
func (e *EffectExecutor) complete(ctx context.Context, r EffectRequest, receipt EffectReceipt) (EffectReceipt, error) {
	if ctx.Err() != nil || e.store.CompleteEffect(ctx, cloneEffect(r), receipt) != nil {
		return EffectReceipt{}, ErrEffectUncertain
	}
	if e.authorizeRequest(ctx, r, EffectActionReconcile) != nil {
		return EffectReceipt{}, ErrEffectUncertain
	}
	return receipt, nil
}
func (e *EffectExecutor) authorizeRequest(ctx context.Context, r EffectRequest, action EffectAction) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := e.authorize(ctx, cloneEffect(r), action); err != nil {
		return err
	}
	return ctx.Err()
}
func cloneEffect(r EffectRequest) EffectRequest {
	r.Scope.Levels = append([]cortex.Level(nil), r.Scope.Levels...)
	return r
}
