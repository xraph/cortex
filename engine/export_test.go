package engine

import (
	"context"

	"github.com/xraph/cortex/id"
)

// ExportResolveSession exposes the unexported resolveSession method to
// external tests in engine_test.
//
//nolint:revive // ctx as second param matches the call sites required by the session tests; test-only shim, not a public API
func ExportResolveSession(e *Engine, ctx context.Context, a id.AgentID, o id.SessionID) (id.SessionID, error) {
	return e.resolveSession(ctx, a, o)
}

// PauseLeaseRenewals stops or restarts the heartbeat of every lease e
// holds, as a worker stalled in a GC pause or on a slow store would.
func PauseLeaseRenewals(e *Engine, paused bool) { e.renewalsPaused.Store(paused) }

// SweepOnce runs one pass of e's sweeper.
//
//nolint:revive // e first, like every other shim here
func SweepOnce(e *Engine, ctx context.Context) { e.sweepOnce(ctx) }
