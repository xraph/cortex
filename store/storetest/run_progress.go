package storetest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/suspension"
)

// RunProgress proves the store half of continuing a run after its worker
// dies. A worker saves the run's progress at each durable boundary, only
// while it still holds the run's lease, and a replica that reclaims the run
// reads that snapshot back exactly as it was written.
func RunProgress(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Run("RoundTrip", func(t *testing.T) { testProgressRoundTrip(t, newStore) })
	t.Run("Upsert", func(t *testing.T) { testProgressUpsert(t, newStore) })
	t.Run("OwnerFence", func(t *testing.T) { testProgressOwnerFence(t, newStore) })
	t.Run("ExactScope", func(t *testing.T) { testProgressExactScope(t, newStore) })
	t.Run("DeleteIdempotent", func(t *testing.T) { testProgressDeleteIdempotent(t, newStore) })
}

// RunProgressCascade proves that a run's progress row goes when the run
// does. Stores have no method that deletes a run, so the backend passes
// deleteRun, which removes the run row the way an operator would. Mongo
// has no foreign keys and does not run it.
func RunProgressCascade(t *testing.T, s store.Store, deleteRun func(context.Context, id.AgentRunID) error) {
	t.Run("CascadesWithRun", func(t *testing.T) {
		ps := progressStore(t, s)
		ctx := ctxWithScope("progress-cascade")
		lease := leaseAt("w1", time.Now().Add(time.Minute))
		r := mustRunningRun(t, s, ctx, &lease)
		if err := ps.SaveProgress(ctx, progressFixture(r.ID, "w1")); err != nil {
			t.Fatalf("SaveProgress: %v", err)
		}
		if err := deleteRun(ctx, r.ID); err != nil {
			t.Fatalf("delete run: %v", err)
		}
		if _, err := s.GetRun(ctx, r.ID); !errors.Is(err, cortex.ErrRunNotFound) {
			t.Fatalf("GetRun after deleting the run = %v, want ErrRunNotFound", err)
		}
		if _, err := ps.GetProgress(ctx, r.ID); !errors.Is(err, cortex.ErrProgressNotFound) {
			t.Fatalf("GetProgress after deleting the run = %v, want ErrProgressNotFound", err)
		}
	})
}

func progressStore(t *testing.T, s store.Store) suspension.ProgressStore {
	t.Helper()
	ps, ok := s.(suspension.ProgressStore)
	if !ok {
		t.Fatalf("%T does not implement suspension.ProgressStore", s)
	}
	return ps
}

// progressFixture is a snapshot with everything a store could lose: the
// prepared bytes with odd spacing and key order, a budget deadline finer
// than a millisecond, a started call and two finished ones.
func progressFixture(runID id.AgentRunID, owner string) *suspension.Progress {
	cont := newSuspension(runID).Cont
	cont.SchemaVersion = 2
	cont.Prepared = []byte(`{"z": 1,  "a":[2, {"y":"x"}]}`)
	cont.Config.Budget = execution.Budget{MaxSteps: 9, MaxTokens: 500, Deadline: time.Now().UTC().Truncate(time.Second).Add(time.Hour + 123456789*time.Nanosecond)}
	return &suspension.Progress{
		RunID: runID,
		Owner: owner,
		Cont:  cont,
		Started: []suspension.PendingCall{
			{ID: "call_deploy", Name: "deploy", Arguments: `{"env": "prod",  "replicas":3}`},
		},
		Done: []suspension.ProgressResult{
			{CallID: "call_read", Content: `{"status":"ok"}`, Outcome: "completed"},
			{CallID: "call_drop", Error: "denied by policy", Outcome: "denied"},
		},
	}
}

// sameProgress compares two snapshots with UpdatedAt set aside.
func sameProgress(got, want *suspension.Progress) bool {
	if got == nil || want == nil {
		return got == want
	}
	a, b := *got, *want
	a.UpdatedAt, b.UpdatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

func mustGetProgress(t *testing.T, ps suspension.ProgressStore, ctx context.Context, runID id.AgentRunID) *suspension.Progress { //nolint:revive // t leads every test helper in this file; ctx after the store matches that convention
	t.Helper()
	got, err := ps.GetProgress(ctx, runID)
	if err != nil {
		t.Fatalf("GetProgress: %v", err)
	}
	return got
}

func testProgressRoundTrip(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	ps := progressStore(t, s)
	ctx := ctxWithScope("progress", "round-trip")
	lease := leaseAt("w1", time.Now().Add(time.Minute))
	r := mustRunningRun(t, s, ctx, &lease)

	saved := progressFixture(r.ID, "w1")
	want := *saved
	want.Scope = scopeOf("progress", "round-trip")
	digest := evidence.Digest(saved.Cont.Prepared)
	before := time.Now().Add(-time.Second)
	if err := ps.SaveProgress(ctx, saved); err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}

	got := mustGetProgress(t, ps, ctx, r.ID)
	if !sameProgress(got, &want) {
		t.Fatalf("GetProgress returned\n%+v\nwant\n%+v", got, &want)
	}
	if evidence.Digest(got.Cont.Prepared) != digest {
		t.Fatalf("prepared bytes came back as %q, digest changed", got.Cont.Prepared)
	}
	if got.UpdatedAt.IsZero() || got.UpdatedAt.Before(before) {
		t.Fatalf("UpdatedAt = %v, want the time of the save", got.UpdatedAt)
	}
}

func testProgressUpsert(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	ps := progressStore(t, s)
	ctx := ctxWithScope("progress-upsert")
	lease := leaseAt("w1", time.Now().Add(time.Minute))
	r := mustRunningRun(t, s, ctx, &lease)

	first := progressFixture(r.ID, "w1")
	if err := ps.SaveProgress(ctx, first); err != nil {
		t.Fatalf("first SaveProgress: %v", err)
	}
	stored := mustGetProgress(t, ps, ctx, r.ID)

	// The next boundary: the started call finished and a new step began.
	second := progressFixture(r.ID, "w1")
	second.Cont.StepIndex = 3
	second.Started = nil
	second.Done = []suspension.ProgressResult{{CallID: "call_deploy", Content: "deployed", Outcome: "completed"}}
	if err := ps.SaveProgress(ctx, second); err != nil {
		t.Fatalf("second SaveProgress: %v", err)
	}
	got := mustGetProgress(t, ps, ctx, r.ID)
	if got.Cont.StepIndex != 3 || len(got.Started) != 0 || len(got.Done) != 1 || got.Done[0] != second.Done[0] {
		t.Fatalf("after the second save: step %d, started %+v, done %+v; want step 3, nothing started, %+v", got.Cont.StepIndex, got.Started, got.Done, second.Done)
	}
	if got.UpdatedAt.Before(stored.UpdatedAt) {
		t.Fatalf("UpdatedAt went back from %v to %v", stored.UpdatedAt, got.UpdatedAt)
	}
}

func testProgressOwnerFence(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	ps := progressStore(t, s)
	ctx := ctxWithScope("progress-fence")
	lease := leaseAt("w1", time.Now().Add(time.Minute))
	r := mustRunningRun(t, s, ctx, &lease)
	held := progressFixture(r.ID, "w1")
	if err := ps.SaveProgress(ctx, held); err != nil {
		t.Fatalf("SaveProgress by the lease owner: %v", err)
	}
	kept := mustGetProgress(t, ps, ctx, r.ID)

	stale := progressFixture(r.ID, "w2")
	stale.Done = nil
	if err := ps.SaveProgress(ctx, stale); !errors.Is(err, cortex.ErrRunLeaseLost) {
		t.Fatalf("SaveProgress by another owner = %v, want ErrRunLeaseLost", err)
	}
	if got := mustGetProgress(t, ps, ctx, r.ID); got.Owner != "w1" || !sameProgress(got, kept) {
		t.Fatalf("a refused save changed the snapshot to owner %q done %+v", got.Owner, got.Done)
	}
	if err := ps.SaveProgress(ctx, progressFixture(r.ID, "")); !errors.Is(err, cortex.ErrRunLeaseLost) {
		t.Fatalf("SaveProgress with no owner = %v, want ErrRunLeaseLost", err)
	}

	// A run claimed with no lease has lease_owner ''. A save with no owner
	// must not match it.
	unleased := mustRunningRun(t, s, ctx, nil)
	if err := ps.SaveProgress(ctx, progressFixture(unleased.ID, "")); !errors.Is(err, cortex.ErrRunLeaseLost) {
		t.Fatalf("SaveProgress with no owner on a run with no lease = %v, want ErrRunLeaseLost", err)
	}
	if _, err := ps.GetProgress(ctx, unleased.ID); !errors.Is(err, cortex.ErrProgressNotFound) {
		t.Fatalf("a refused save on a run with no lease wrote a snapshot: %v", err)
	}
	// A run whose lease has an expiry but no owner reads back with an
	// empty owner. An empty owner must not match that either.
	ownerless := run.Lease{ExpiresAt: time.Now().Add(time.Minute).UTC().Truncate(time.Millisecond)}
	unowned := mustRunningRun(t, s, ctx, &ownerless)
	if err := ps.SaveProgress(ctx, progressFixture(unowned.ID, "")); !errors.Is(err, cortex.ErrRunLeaseLost) {
		t.Fatalf("SaveProgress with no owner on a lease with no owner = %v, want ErrRunLeaseLost", err)
	}
	if _, err := ps.GetProgress(ctx, unowned.ID); !errors.Is(err, cortex.ErrProgressNotFound) {
		t.Fatalf("a refused save on a lease with no owner wrote a snapshot: %v", err)
	}

	paused, _ := mustSuspendRun(t, s, ctx, futureDeadline())
	if err := ps.SaveProgress(ctx, progressFixture(paused.ID, "w1")); !errors.Is(err, cortex.ErrRunLeaseLost) {
		t.Fatalf("SaveProgress on a paused run = %v, want ErrRunLeaseLost", err)
	}
	if _, err := ps.GetProgress(ctx, paused.ID); !errors.Is(err, cortex.ErrProgressNotFound) {
		t.Fatalf("a refused save on a paused run wrote a snapshot: %v", err)
	}
	// The state is checked on its own, not only through the lease: a run
	// row that left running with its lease columns still set (CreateRun
	// writes whatever lease it is given) is not w1's to write.
	leftover := newRun(mustCreateAgent(t, s, ctx, ""))
	leftover.State, leftover.Lease = run.StatePaused, &lease
	if err := s.CreateRun(ctx, leftover); err != nil {
		t.Fatalf("fixture: create paused run with a lease: %v", err)
	}
	if err := ps.SaveProgress(ctx, progressFixture(leftover.ID, "w1")); !errors.Is(err, cortex.ErrRunLeaseLost) {
		t.Fatalf("SaveProgress on a paused run that still names w1 = %v, want ErrRunLeaseLost", err)
	}
	if err := ps.SaveProgress(ctx, progressFixture(id.NewAgentRunID(), "w1")); !errors.Is(err, cortex.ErrRunLeaseLost) {
		t.Fatalf("SaveProgress for a run that does not exist = %v, want ErrRunLeaseLost", err)
	}

	// The worker hands the run off. Its next save is refused.
	if _, err := continuationStore(t, s).MarkReclaimable(ctx, r.ID, time.Now().UTC(), "w1"); err != nil {
		t.Fatalf("hand-off: %v", err)
	}
	if err := ps.SaveProgress(ctx, held); !errors.Is(err, cortex.ErrRunLeaseLost) {
		t.Fatalf("SaveProgress after the run left running = %v, want ErrRunLeaseLost", err)
	}
	if got := mustGetProgress(t, ps, ctx, r.ID); !sameProgress(got, kept) {
		t.Fatalf("a refused save after hand-off changed the snapshot: %+v", got)
	}
}

func testProgressExactScope(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	ps := progressStore(t, s)
	ctx := ctxWithScope("ws_progress", "p1")
	lease := leaseAt("w1", time.Now().Add(time.Minute))
	r := mustRunningRun(t, s, ctx, &lease)
	if err := ps.SaveProgress(ctx, progressFixture(r.ID, "w1")); err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}
	kept := mustGetProgress(t, ps, ctx, r.ID)

	others := map[string]context.Context{
		"covering": ctxWithScope("ws_progress"),
		"sibling":  ctxWithScope("ws_progress", "p2"),
		"other":    ctxWithScope("ws_other", "p1"),
	}
	for name, other := range others {
		if _, err := ps.GetProgress(other, r.ID); !errors.Is(err, cortex.ErrProgressNotFound) {
			t.Errorf("GetProgress from the %s scope = %v, want ErrProgressNotFound", name, err)
		}
		if err := ps.SaveProgress(other, progressFixture(r.ID, "w1")); !errors.Is(err, cortex.ErrRunLeaseLost) {
			t.Errorf("SaveProgress from the %s scope = %v, want ErrRunLeaseLost", name, err)
		}
		if err := ps.DeleteProgress(other, r.ID); err != nil {
			t.Errorf("DeleteProgress from the %s scope = %v, want nil", name, err)
		}
	}
	if got := mustGetProgress(t, ps, ctx, r.ID); !sameProgress(got, kept) || !got.UpdatedAt.Equal(kept.UpdatedAt) {
		t.Fatalf("another scope changed the snapshot: %+v", got)
	}
	if _, err := ps.GetProgress(context.Background(), r.ID); !errors.Is(err, cortex.ErrNoScope) {
		t.Fatalf("GetProgress with no scope = %v, want ErrNoScope", err)
	}
}

func testProgressDeleteIdempotent(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	ps := progressStore(t, s)
	ctx := ctxWithScope("progress-delete")
	lease := leaseAt("w1", time.Now().Add(time.Minute))
	r := mustRunningRun(t, s, ctx, &lease)
	if err := ps.SaveProgress(ctx, progressFixture(r.ID, "w1")); err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}
	for i := range 2 {
		if err := ps.DeleteProgress(ctx, r.ID); err != nil {
			t.Fatalf("DeleteProgress #%d: %v", i+1, err)
		}
	}
	if _, err := ps.GetProgress(ctx, r.ID); !errors.Is(err, cortex.ErrProgressNotFound) {
		t.Fatalf("GetProgress after delete = %v, want ErrProgressNotFound", err)
	}
	if err := ps.DeleteProgress(ctx, id.NewAgentRunID()); err != nil {
		t.Fatalf("DeleteProgress for a run that never saved one = %v, want nil", err)
	}
	if _, err := s.GetRun(ctx, r.ID); err != nil {
		t.Fatalf("DeleteProgress touched the run: %v", err)
	}
}
