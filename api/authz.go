package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/xraph/forge"

	"github.com/xraph/cortex"
)

// Operation is one route of the module, as an Authorizer is asked about
// it.
type Operation struct {
	// ID is the route's OpenAPI operation id, such as "resolveCheckpoint".
	ID string
	// Write is true when the route's method is not GET or HEAD.
	Write bool
}

// Authorizer is the host's say over who may call the module.
//
// Without one, every route is open to whoever reaches the router and
// resolveCheckpoint records the decided_by the request body names, so
// the module is then fit only for a single-trust deployment, where every
// caller is trusted equally.
type Authorizer interface {
	// Authorize is asked before every route runs, with the request's
	// context. A nil error lets the request through. Any other error
	// refuses it with 403, unless it wraps a cortex not-found or scope
	// error, which keeps the status that error has everywhere else in
	// the module. The error's text never reaches the response.
	Authorize(ctx context.Context, op Operation) error
	// DecidedBy is the name resolveCheckpoint records as the deciding
	// person, derived from the request's principal. It replaces any
	// decided_by the request body names.
	DecidedBy(ctx context.Context) (string, error)
}

// WithAuthorizer guards every route with a and has resolveCheckpoint
// record the decider a names rather than the one the body asserts.
func WithAuthorizer(a Authorizer) Option {
	return func(api *API) { api.authorizer = a }
}

// errNotAuthorized is the only text a refused request is told.
const errNotAuthorized = "cortex: operation not authorized"

// routeOptions is a []forge.RouteOption that is itself a route option,
// so a.op can stand in the argument list where forge.WithOperationID
// stood.
type routeOptions []forge.RouteOption

// Apply applies every option in order.
func (o routeOptions) Apply(config *forge.RouteConfig) {
	for _, option := range o {
		option.Apply(config)
	}
}

// op is a route's operation id together with the guard that asks the
// authorizer about it. Every route the module registers takes its id
// from here, so none can be added without a guard.
func (a *API) op(id string, write bool) routeOptions {
	return routeOptions{
		forge.WithOperationID(id),
		forge.WithMiddleware(a.guard(Operation{ID: id, Write: write})),
	}
}

// guard asks the authorizer about op before the route runs. With no
// authorizer it lets everything through.
func (a *API) guard(op Operation) forge.Middleware {
	return func(next forge.Handler) forge.Handler {
		return func(ctx forge.Context) error {
			if a.authorizer == nil {
				return next(ctx)
			}
			if err := a.authorizer.Authorize(ctx.Context(), op); err != nil {
				return authorizationError(err)
			}
			return next(ctx)
		}
	}
}

// authorizationError is the response to an authorizer's refusal. A
// cortex not-found or scope error keeps its usual status, with the
// sentinel's own text rather than the authorizer's. Anything else is a
// 403 that says only that the operation was refused.
func authorizationError(err error) error {
	if sentinel := notFoundSentinel(err); sentinel != nil {
		return forge.NotFound(sentinel.Error())
	}
	if errors.Is(err, cortex.ErrInvalidScope) || errors.Is(err, cortex.ErrUnsupportedScopeDepth) || errors.Is(err, cortex.ErrNoScope) {
		return mapStoreError(scopeSentinel(err))
	}
	return forge.NewHTTPError(http.StatusForbidden, errNotAuthorized)
}

// scopeSentinel is the scope sentinel err wraps, so mapStoreError picks
// the status without seeing the rest of err.
func scopeSentinel(err error) error {
	for _, sentinel := range []error{cortex.ErrNoScope, cortex.ErrUnsupportedScopeDepth, cortex.ErrInvalidScope} {
		if errors.Is(err, sentinel) {
			return sentinel
		}
	}
	return nil
}

// decidedBy is the decider resolveCheckpoint records: the authorizer's
// name for the request principal when there is an authorizer, and the
// body's decided_by when there is none.
func (a *API) decidedBy(ctx context.Context, asserted string) (string, error) {
	if a.authorizer == nil {
		return asserted, nil
	}
	name, err := a.authorizer.DecidedBy(ctx)
	if err != nil {
		return "", authorizationError(err)
	}
	return name, nil
}
