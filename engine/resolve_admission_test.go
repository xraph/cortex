package engine

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
)

// requireUndecided checks that a refused ResolveCheckpoint left everything
// as it found it: the run paused, its checkpoint pending, the gated tool
// never run.
func requireUndecided(t *testing.T, f *continueFixture, runID run.Run, cp *checkpoint.Checkpoint) {
	t.Helper()
	require.Equal(t, run.StatePaused, f.runState(t, runID.ID).State, "a refused decision moved the run")
	require.Equal(t, checkpointStatePending, f.checkpointState(t, cp.ID).State, "a refused decision was recorded")
	require.Zero(t, f.ran.Load())
	require.Empty(t, f.ready.runs, "a refused decision announced a ready run")
}

// An approval that arrives while the engine drains is refused before
// anything is recorded, as in v1.9.0, so the caller can take it to another
// replica. Once the engine admits runs again, the same approval goes
// through.
func TestResolveCheckpointDuringDrainRecordsNothing(t *testing.T) {
	f := newContinueFixture(t)
	paused, cp := f.pause(t)
	require.NoError(t, f.e.Stop(context.Background()))

	err := f.e.ResolveCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"})
	require.ErrorIs(t, err, cortex.ErrEngineStopping)
	require.ErrorContains(t, err, "nothing was recorded")
	requireUndecided(t, f, *paused, cp)

	require.NoError(t, f.e.Start(context.Background()))
	t.Cleanup(func() { _ = f.e.Stop(context.Background()) })
	require.NoError(t, f.e.ResolveCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))
	require.Equal(t, run.StateCompleted, f.runState(t, paused.ID).State)
	require.EqualValues(t, 1, f.ran.Load())
}

// Under a full run_concurrency cap, an approval whose caller gives up
// waiting for a slot records nothing either.
func TestResolveCheckpointRefusedForASlotRecordsNothing(t *testing.T) {
	f := newContinueFixture(t, WithRunConcurrency(1))
	paused, cp := f.pause(t)
	_, release, err := f.e.admit(context.Background())
	require.NoError(t, err)

	deadline, cancel := context.WithTimeout(as(f.ctx, bob), 50*time.Millisecond)
	defer cancel()
	err = f.e.ResolveCheckpoint(deadline, cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, "nothing was recorded")
	requireUndecided(t, f, *paused, cp)

	release()
	require.NoError(t, f.e.ResolveCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))
	require.Equal(t, run.StateCompleted, f.runState(t, paused.ID).State)
}

// A rejection and a checkpoint the host wrote itself continue nothing, so
// they need no slot and are recorded even while the engine drains, as in
// v1.9.0.
func TestResolveCheckpointRejectionDuringDrainIsRecorded(t *testing.T) {
	f := newContinueFixture(t)
	paused, cp := f.pause(t)
	require.NoError(t, f.e.Stop(context.Background()))

	require.NoError(t, f.e.ResolveCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{DecidedBy: "bob", Reason: "no"}))
	require.Equal(t, run.StateFailed, f.runState(t, paused.ID).State)
	require.NotEqual(t, checkpointStatePending, f.checkpointState(t, cp.ID).State)
	require.Zero(t, f.ran.Load())
}

// With leasing on, the run an approval continues is the caller's own run,
// not one nested inside another: when Stop drains, it hands off at its
// next boundary, and ResolveCheckpoint reports the hand-off.
func TestResolveCheckpointContinuationHandsOffWhileDraining(t *testing.T) {
	entered, gate := make(chan struct{}), make(chan struct{})
	block := WithTool(llm.Tool{Name: "block", Description: "waits for the test"}, func(context.Context, cortex.Invocation) (string, error) {
		close(entered)
		<-gate
		return "blocked", nil
	})
	f := newContinueFixture(t, WithRunLease(60*time.Second), block)
	f.model.then = [][]llm.ToolCall{{{ID: "call-2", Name: "block", Arguments: `{}`}}}
	f.overrides = &RunOverrides{MaxSteps: 5}
	paused, cp := f.pause(t)

	resolved := make(chan error, 1)
	go func() {
		resolved <- f.e.ResolveCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"})
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the continued run never reached its blocking tool")
	}

	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stopped <- f.e.Stop(ctx)
	}()
	require.Eventually(t, f.e.admission.draining, 5*time.Second, 5*time.Millisecond)
	close(gate)

	select {
	case err := <-resolved:
		require.ErrorIs(t, err, cortex.ErrRunInterrupted, "the continued run was not handed off")
	case <-time.After(10 * time.Second):
		t.Fatal("ResolveCheckpoint did not return")
	}
	require.NoError(t, <-stopped)
	require.Equal(t, run.StateReclaimable, f.runState(t, paused.ID).State)
	require.Equal(t, "resolved", f.checkpointState(t, cp.ID).State)
	require.EqualValues(t, 1, f.ran.Load())
}
