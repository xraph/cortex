package orchestrationstate

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

type faultDriver struct {
	snapshot Snapshot
	writes   int
	write    func(*orchestration.Run) (bool, error)
}

func (d *faultDriver) Read(context.Context, id.OrchestrationID) (Snapshot, error) {
	return Snapshot{Run: clone(d.snapshot.Run), Revision: d.snapshot.Revision}, nil
}

func (d *faultDriver) CompareAndSwap(_ context.Context, _ Snapshot, after *orchestration.Run) (bool, error) {
	d.writes++
	return d.write(after)
}

func TestLostAcknowledgmentDoesNotRetryAdmission(t *testing.T) {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "fault"}}}
	ctx := cortex.WithScope(context.Background(), scope)
	team := &orchestration.Run{ID: id.NewOrchestrationID(), Scope: scope, Status: orchestration.StatusRunning}
	d := &faultDriver{snapshot: Snapshot{Run: team}}
	lost := errors.New("acknowledgment lost")
	d.write = func(after *orchestration.Run) (bool, error) {
		d.snapshot = Snapshot{Run: clone(after), Revision: 1}
		return false, lost
	}
	child := id.NewAgentRunID()
	require.ErrorIs(t, Link(ctx, d, team.ID, child), lost)
	require.Equal(t, 1, d.writes)
	require.Equal(t, []id.AgentRunID{child}, d.snapshot.Run.AgentRunIDs)
	require.ErrorIs(t, Link(ctx, d, team.ID, child), cortex.ErrAlreadyExists)
	require.Equal(t, 1, d.writes)
}

func TestRevisionExhaustionAndContentionFailClosed(t *testing.T) {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "contention"}}}
	ctx := cortex.WithScope(context.Background(), scope)
	for _, revision := range []int64{-1, math.MaxInt64, 0} {
		d := &faultDriver{snapshot: Snapshot{Run: &orchestration.Run{ID: id.NewOrchestrationID(), Scope: scope, Status: orchestration.StatusRunning}, Revision: revision}}
		d.write = func(*orchestration.Run) (bool, error) { return false, nil }
		require.ErrorIs(t, Link(ctx, d, d.snapshot.Run.ID, id.NewAgentRunID()), cortex.ErrOrchestrationRunConflict)
		if revision != 0 {
			require.Zero(t, d.writes)
		} else {
			require.Positive(t, d.writes)
			require.LessOrEqual(t, d.writes, 64)
		}
		require.Empty(t, d.snapshot.Run.AgentRunIDs)
	}
}
