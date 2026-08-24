package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/scopespy"
)

// TestCancelRun_APausedRunIsClaimedAndCleanedUp is the ordinary path. A
// cancelled run keeps nothing back: the suspension goes, because no claim
// will ever take it again, and the checkpoint goes with it, because an
// operator asked to approve a run that has already ended has been handed
// a decision that cannot mean anything.
func TestCancelRun_APausedRunIsClaimedAndCleanedUp(t *testing.T) {
	spy := scopespy.New()
	e := approvalEngine(t, WithStore(spy))
	ctx := cortex.WithScope(context.Background(), approvalScope())

	paused, err := e.RunAgent(ctx, "assistant", "clean up", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if paused.State != run.StatePaused {
		t.Fatalf("fixture run state = %q, want %q", paused.State, run.StatePaused)
	}

	if cancelErr := e.CancelRun(ctx, paused.ID); cancelErr != nil {
		t.Fatalf("CancelRun: %v", cancelErr)
	}

	stored, err := spy.GetRun(ctx, paused.ID)
	if err != nil {
		t.Fatalf("reload the run: %v", err)
	}
	if stored.State != run.StateCancelled {
		t.Errorf("run state = %q, want %q", stored.State, run.StateCancelled)
	}
	if got := len(spy.Suspensions()); got != 0 {
		t.Errorf("%d suspensions left, want 0", got)
	}

	cps := spy.Checkpoints()
	if len(cps) != 1 {
		t.Fatalf("the run wrote %d checkpoints, want 1", len(cps))
	}
	if cps[0].State == "pending" {
		t.Error("the cancelled run left its checkpoint pending; somebody is still being asked to decide it")
	}
	if cps[0].Decision == nil || cps[0].Decision.Approved {
		t.Errorf("checkpoint decision = %+v, want a rejection recorded against the cancel", cps[0].Decision)
	}
}

// TestCancelRun_LosesToAWriterThatAlreadyClaimedTheRun is the reason the
// claim is there at all.
//
// Cancel used to read the run, see paused, and write cancelled with
// nothing in between. A resume that had already claimed the same run got
// its state stomped, and then wrote its own terminal state back over the
// cancel a moment later: the operator was told the run was cancelled, and
// the run went on to finish. The claim here stands in for that resume,
// which is exactly what it does to the row.
func TestCancelRun_LosesToAWriterThatAlreadyClaimedTheRun(t *testing.T) {
	spy := scopespy.New()
	e := approvalEngine(t, WithStore(spy))
	ctx := cortex.WithScope(context.Background(), approvalScope())

	paused, err := e.RunAgent(ctx, "assistant", "clean up", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	// Somebody else takes the run first.
	if _, claimErr := spy.ClaimSuspension(ctx, paused.ID); claimErr != nil {
		t.Fatalf("claim the suspension out from under the cancel: %v", claimErr)
	}

	err = e.CancelRun(ctx, paused.ID)
	if !errors.Is(err, cortex.ErrNotSuspended) {
		t.Fatalf("CancelRun against a run somebody else claimed = %v, want ErrNotSuspended", err)
	}

	stored, getErr := spy.GetRun(ctx, paused.ID)
	if getErr != nil {
		t.Fatalf("reload the run: %v", getErr)
	}
	if stored.State == run.StateCancelled {
		t.Error("the cancel stomped a run another writer already owned; whatever that writer does next lands on top of it")
	}
	if got := len(spy.Suspensions()); got != 1 {
		t.Errorf("%d suspensions left, want 1: the losing cancel must not clean up after the winner", got)
	}
	cps := spy.Checkpoints()
	if len(cps) != 1 || cps[0].State != "pending" {
		t.Errorf("checkpoints = %+v, want one still pending", cps)
	}
}

// TestCancelRun_ARunThatAlreadyEndedIsRefused keeps the state guard the
// REST handler used to hold. It moved into the engine with the rest of
// the cancel, and the error has to stay matchable, because that is what
// still answers a caller with 400 rather than 500.
func TestCancelRun_ARunThatAlreadyEndedIsRefused(t *testing.T) {
	spy := scopespy.New()
	e := mustResumeEngine(t, spy)
	ctx := cortex.WithScope(context.Background(), scopeA())

	r := &run.Run{
		Entity: cortex.NewEntity(),
		ID:     id.NewAgentRunID(),
		Scope:  scopeA(),
		State:  run.StateCompleted,
	}
	if err := spy.CreateRun(ctx, r); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	err := e.CancelRun(ctx, r.ID)
	if !errors.Is(err, cortex.ErrInvalidState) {
		t.Fatalf("CancelRun on a completed run = %v, want ErrInvalidState", err)
	}

	stored, getErr := spy.GetRun(ctx, r.ID)
	if getErr != nil {
		t.Fatalf("reload the run: %v", getErr)
	}
	if stored.State != run.StateCompleted {
		t.Errorf("run state = %q, want %q left alone", stored.State, run.StateCompleted)
	}
}

// TestClosePendingCheckpoints_TouchesOnlyItsOwnRun pins the filter the
// cleanup runs on. A filter that quietly matched everything would resolve
// other runs' checkpoints too, and no assertion about the cancelled run
// itself would ever notice.
func TestClosePendingCheckpoints_TouchesOnlyItsOwnRun(t *testing.T) {
	spy := scopespy.New()
	ctx := cortex.WithScope(context.Background(), approvalScope())

	// Two engines over one store, because the LLM double answers with
	// tool calls once and plainly after: a second run on the same engine
	// would never pause.
	e := approvalEngine(t, WithStore(spy))
	first, err := e.RunAgent(ctx, "assistant", "clean up", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	second, err := approvalEngine(t, WithStore(spy)).RunAgent(ctx, "assistant", "clean up again", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if len(spy.Checkpoints()) != 2 {
		t.Fatalf("the two runs wrote %d checkpoints, want 2", len(spy.Checkpoints()))
	}

	if cancelErr := e.CancelRun(ctx, first.ID); cancelErr != nil {
		t.Fatalf("CancelRun: %v", cancelErr)
	}

	for _, cp := range spy.Checkpoints() {
		switch cp.RunID {
		case first.ID:
			if cp.State == "pending" {
				t.Error("the cancelled run's checkpoint is still pending")
			}
		case second.ID:
			if cp.State != "pending" {
				t.Errorf("the other run's checkpoint is %q; cancelling one run must not decide another's", cp.State)
			}
		default:
			t.Errorf("unexpected checkpoint on run %s", cp.RunID)
		}
	}

	pending, err := e.ListPendingCheckpoints(ctx, &checkpoint.ListFilter{})
	if err != nil {
		t.Fatalf("ListPendingCheckpoints: %v", err)
	}
	if len(pending) != 1 || pending[0].RunID != second.ID {
		t.Errorf("pending queue = %+v, want only the run that is still waiting", pending)
	}
}
