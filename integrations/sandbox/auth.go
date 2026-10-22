// Package sandboxbroker serves authenticated, durable sandbox jobs.
package sandboxbroker

import (
	"context"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/sandbox"
)

var (
	ErrDenied   = errors.New("cortex: sandbox access denied")
	ErrNotFound = errors.New("cortex: sandbox job not found")
	ErrConflict = errors.New("cortex: sandbox job identity conflict")
	ErrPending  = errors.New("cortex: sandbox result pending")
)

// Identity comes from authenticated host transport, never from request JSON.
type Identity struct {
	Scope     cortex.Scope
	Principal string
}
type identityKey struct{}

// WithIdentity records a detached, authenticated caller for trusted hosts.
func WithIdentity(ctx context.Context, who Identity) context.Context {
	who.Scope.Levels = append([]cortex.Level(nil), who.Scope.Levels...)
	return context.WithValue(ctx, identityKey{}, who)
}
func caller(ctx context.Context) (Identity, error) {
	who, ok := ctx.Value(identityKey{}).(Identity)
	if !ok || who.Principal == "" {
		return Identity{}, ErrDenied
	}
	if err := cortex.ValidateScope(who.Scope); err != nil {
		return Identity{}, ErrDenied
	}
	who.Scope.Levels = append([]cortex.Level(nil), who.Scope.Levels...)
	return who, ctx.Err()
}

// Authorizer rechecks current host grants for every operation, including retries.
// Scope and principal equality are also enforced by the broker itself.
type Authorizer func(context.Context, Identity, sandbox.Job) error
