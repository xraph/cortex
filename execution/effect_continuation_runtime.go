package execution

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/internal/hostport"
)

// EffectContinuationAuthority authenticates provider proof and current grants.
// WithAuthority holds permanent original-worker exclusion and current grant
// guards through the callback. Authorize must independently permit compensation,
// verify predecessor proof and enforce any monetary ceiling at the provider.
// Reconciliation authorizes reads only and never grants replay of a mutation.
type EffectContinuationAuthority interface {
	WithEffectContinuationAuthority(context.Context, EffectContinuationRequest, func(EffectContinuationRequest) error) error
	AuthorizeEffectContinuation(context.Context, EffectContinuationRequest, EffectAction) error
}

// ContinueEffect admits an independent attempt before any adapter dispatch.
// Only an acknowledged fresh admission permits invocation. A duplicate or lost
// acknowledgment requires observation-only recovery. It never resumes the run.
func (e *EffectExecutor) ContinueEffect(ctx context.Context, request EffectContinuationRequest, args []byte, authority EffectContinuationAuthority) (EffectContinuationResult, error) {
	if len(args) > 64<<10 || evidence.Digest(args) != request.Operation.ArgsDigest {
		return EffectContinuationResult{}, ErrInvalidEffect
	}
	return e.continueEffect(ctx, request, append([]byte(nil), args...), authority, false)
}

// ReconcileContinuation observes one retained attempt without dispatching it.
func (e *EffectExecutor) ReconcileContinuation(ctx context.Context, request EffectContinuationRequest, authority EffectContinuationAuthority) (EffectContinuationResult, error) {
	return e.continueEffect(ctx, request, nil, authority, true)
}

func (e *EffectExecutor) continueEffect(ctx context.Context, request EffectContinuationRequest, args []byte, authority EffectContinuationAuthority, recoverOnly bool) (EffectContinuationResult, error) {
	var zero EffectContinuationResult
	if effectNil(authority) {
		return zero, ErrEffectUnavailable
	}
	store, ok := e.store.(EffectContinuationStore)
	if !ok {
		return zero, ErrEffectUnavailable
	}
	req, digest, err := request.Freeze()
	if err != nil {
		return zero, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return zero, err
	}
	if scope.Canonical() != req.Original.Scope.Canonical() {
		return zero, cortex.ErrInvalidScope
	}
	registration, ok := e.adapters[req.Operation.Requirement.Tool]
	if !ok || registration.Requirement != req.Operation.Requirement {
		return zero, ErrInvalidEffect
	}
	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	default:
		return zero, ErrEffectUnavailable
	}
	bounded, cancel := context.WithTimeout(ctx, MaxEffectDuration)
	defer cancel()
	offered, _, err := req.Freeze()
	if err != nil {
		return zero, err
	}
	return hostport.WithSynchronousFence(bounded, ErrEffectUnavailable, func(tc context.Context, next func(EffectContinuationRequest) error) error {
		return authority.WithEffectContinuationAuthority(tc, offered, next)
	}, func(tc context.Context, current EffectContinuationRequest) (EffectContinuationResult, error) {
		_, currentDigest, freezeErr := current.Freeze()
		if freezeErr != nil || currentDigest != digest {
			return zero, ErrEffectConflict
		}
		action := req.Action
		if recoverOnly {
			action = EffectActionReconcile
		}
		authorize := func(action EffectAction) error {
			detached, _, copyErr := req.Freeze()
			if copyErr != nil {
				return copyErr
			}
			if authErr := authority.AuthorizeEffectContinuation(tc, detached, action); authErr != nil {
				return authErr
			}
			return e.authorizeRequest(tc, req.Operation, action)
		}
		if authErr := authorize(action); authErr != nil {
			return zero, authErr
		}
		fresh := false
		var admission EffectContinuation
		if !recoverOnly {
			admitted, admitErr := store.AdmitEffectContinuation(tc, req)
			if admitErr != nil {
				return zero, errors.Join(ErrEffectUncertain, admitErr)
			}
			if admitted.Validate() != nil || admitted.RequestDigest != digest {
				return zero, ErrEffectUncertain
			}
			admission = admitted
			fresh = admission.Fresh
		}
		if !fresh {
			if recoverOnly {
				history, readErr := store.ListEffectContinuations(tc, req.Original)
				if readErr != nil {
					return zero, readErr
				}
				for _, entry := range history {
					if entry.RequestDigest == digest && entry.Validate() == nil {
						admission = entry
						break
					}
				}
				if admission.RequestDigest != digest {
					return zero, ErrEffectNotFound
				}
			}
			result, readErr := store.GetEffectContinuationResult(tc, req)
			if readErr != nil {
				return zero, readErr
			}
			if result != nil {
				if result.Validate(admission) != nil {
					return zero, ErrInvalidEffect
				}
				if authErr := authorize(EffectActionReconcile); authErr != nil {
					return zero, authErr
				}
				return *result, nil
			}
			if authErr := authorize(EffectActionReconcile); authErr != nil {
				return zero, authErr
			}
			status, probeErr := registration.Adapter.Reconcile(tc, cloneEffect(req.Operation))
			if probeErr != nil || status.Receipt == nil || status.Outcome != status.Receipt.Outcome || status.Receipt.Validate(req.Operation) != nil {
				return zero, ErrEffectUncertain
			}
			return e.completeContinuation(tc, store, admission, *status.Receipt, authorize)
		}
		if !time.Now().Before(req.Operation.Deadline) {
			return zero, ErrEffectUncertain
		}
		dispatch, finish := context.WithDeadline(tc, req.Operation.Deadline)
		defer finish()
		if authErr := authorize(req.Action); authErr != nil {
			return zero, errors.Join(ErrEffectUncertain, authErr)
		}
		receipt, callErr := registration.Adapter.Execute(dispatch, cloneEffect(req.Operation), args)
		if callErr != nil || receipt.Validate(req.Operation) != nil {
			return zero, ErrEffectUncertain
		}
		return e.completeContinuation(tc, store, admission, receipt, authorize)
	})
}
func (e *EffectExecutor) completeContinuation(ctx context.Context, store EffectContinuationStore, admission EffectContinuation, receipt EffectReceipt, authorize func(EffectAction) error) (EffectContinuationResult, error) {
	if err := ctx.Err(); err != nil {
		return EffectContinuationResult{}, errors.Join(ErrEffectUncertain, err)
	}
	result, err := store.CompleteEffectContinuation(ctx, admission.Request, receipt)
	if err != nil {
		return EffectContinuationResult{}, errors.Join(ErrEffectUncertain, err)
	}
	if result.Validate(admission) != nil {
		return EffectContinuationResult{}, ErrEffectUncertain
	}
	if err = authorize(EffectActionReconcile); err != nil {
		return EffectContinuationResult{}, errors.Join(ErrEffectUncertain, err)
	}
	return result, nil
}
