package optimization

import (
	"context"

	"github.com/xraph/cortex"
)

// ResolveIdentity rebuilds current host identity within the caller's exact scope
// without extending cancellation or deadline. It does not authorize an action;
// callers must check both the caller and resolved identity with their Authorizer.
// The returned cleanup function must be called when the operation finishes.
func ResolveIdentity(ctx context.Context, resolver IdentityResolver, actor IdentityRef, scope cortex.Scope) (context.Context, context.CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if resolver == nil || !ValidOpaqueKey(actor.Subject) {
		return nil, nil, ErrInvalidSpec
	}
	caller := cortex.ScopeFromContext(ctx)
	if caller.IsZero() {
		return nil, nil, cortex.ErrNoScope
	}
	key, err := ScopeKey(scope)
	if err != nil {
		return nil, nil, err
	}
	callerKey, err := ScopeKey(caller)
	if err != nil || callerKey != key {
		return nil, nil, ErrPolicyDenied
	}
	resolved, err := resolver.Resolve(copyScopeContext(ctx), actor, copyScope(scope))
	if err != nil {
		return nil, nil, err
	}
	if resolved == nil {
		return nil, nil, ErrPolicyDenied
	}
	resolvedKey, err := ScopeKey(cortex.ScopeFromContext(resolved))
	if err != nil || resolvedKey != key {
		return nil, nil, ErrPolicyDenied
	}
	var child context.Context
	var cancel context.CancelFunc
	if deadline, exists := ctx.Deadline(); exists {
		child, cancel = context.WithDeadline(copyScopeContext(resolved), deadline)
	} else {
		child, cancel = context.WithCancel(copyScopeContext(resolved))
	}
	stop := context.AfterFunc(ctx, cancel)
	if err = ctx.Err(); err != nil {
		stop()
		cancel()
		return nil, nil, err
	}
	return child, func() { stop(); cancel() }, nil
}

func copyScope(scope cortex.Scope) cortex.Scope {
	scope.Levels = append([]cortex.Level(nil), scope.Levels...)
	return scope
}

func copyScopeContext(ctx context.Context) context.Context {
	return cortex.WithScope(ctx, copyScope(cortex.ScopeFromContext(ctx)))
}
