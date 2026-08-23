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
