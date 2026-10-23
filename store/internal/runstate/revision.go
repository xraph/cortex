package runstate

import (
	"context"
	"math"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

// NextRevision rejects invalid or exhausted versions without wrapping around.
func NextRevision(current int64) (int64, error) {
	if current < 0 || current == math.MaxInt64 {
		return 0, cortex.ErrRunConflict
	}
	return current + 1, nil
}

// UpdateConflict classifies a failed conditional write without exposing a run
// outside the exact caller scope. The read is diagnostic, never write authority.
func UpdateConflict(ctx context.Context, reader interface {
	GetRun(context.Context, id.AgentRunID) (*run.Run, error)
}, scope cortex.Scope, runID id.AgentRunID) error {
	current, err := reader.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if current.Scope.Canonical() != scope.Canonical() {
		return cortex.ErrRunNotFound
	}
	return cortex.ErrRunConflict
}
