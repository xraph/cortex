package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

// RunCandidateLease proves that SetRunCandidate, the run write behind
// suspension claims, resume closure and private candidates, keeps a lease
// only on a running run. A run it moves out of running loses its lease, so
// the sweeper never lists it as lapsed; a run it leaves running keeps the
// lease its worker holds.
func RunCandidateLease(t *testing.T, s store.Store, driver assurance.RunDriver) {
	write := func(t *testing.T, ctx context.Context, r *run.Run, state run.State) {
		t.Helper()
		err := driver.TransactRun(ctx, r.ID, func(tc context.Context, tx assurance.RunTransaction) error {
			current, readErr := tx.GetRun(tc, r.ID)
			if readErr != nil {
				return readErr
			}
			current.State = state
			return tx.SetRunCandidate(tc, current, time.Now().UTC())
		})
		if err != nil {
			t.Fatalf("SetRunCandidate to %s: %v", state, err)
		}
	}

	t.Run("LeavingRunningClearsLease", func(t *testing.T) {
		ctx := ctxWithScope("candidate-lease", "leave")
		lease := leaseAt("w1", time.Now().Add(-time.Minute))
		r := mustRunningRun(t, s, ctx, &lease)
		write(t, ctx, r, run.StateCompleted)
		got := mustGetRun(t, s, ctx, r.ID)
		if got.State != run.StateCompleted || got.Lease != nil {
			t.Fatalf("after SetRunCandidate to completed: state %q lease %+v, want no lease", got.State, got.Lease)
		}
		lapsed, err := continuationStore(t, s).ListLapsedAcrossScopes(context.Background(), time.Now().UTC(), 0)
		if err != nil {
			t.Fatalf("ListLapsedAcrossScopes: %v", err)
		}
		if containsRun(lapsed, r.ID) {
			t.Fatal("a completed run is listed as lapsed")
		}
	})

	t.Run("StayingRunningKeepsLease", func(t *testing.T) {
		ctx := ctxWithScope("candidate-lease", "stay")
		lease := leaseAt("w1", time.Now().Add(time.Minute))
		r := mustRunningRun(t, s, ctx, &lease)
		write(t, ctx, r, run.StateRunning)
		if got := mustGetRun(t, s, ctx, r.ID); got.State != run.StateRunning || !sameLease(got.Lease, lease) {
			t.Fatalf("after SetRunCandidate keeping running: state %q lease %+v, want %+v", got.State, got.Lease, lease)
		}
	})
}
