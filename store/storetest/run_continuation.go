package storetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
)

// RunContinuation proves the store half of non-blocking resume. A decision
// or an answer moves a paused run to ready, any replica claims it once, and
// a worker's lease on a running run either holds, hands off, or lapses into
// reclaimable for the sweeper to give to someone else.
func RunContinuation(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Run("MarkReadyFromPaused", func(t *testing.T) { testMarkReadyFromPaused(t, newStore) })
	t.Run("MarkReadyRefusesOtherStates", func(t *testing.T) { testMarkReadyRefusesOtherStates(t, newStore) })
	t.Run("MarkReadyRefusesExpired", func(t *testing.T) { testMarkReadyRefusesExpired(t, newStore) })
	t.Run("MarkReadyExactScope", func(t *testing.T) { testMarkReadyExactScope(t, newStore) })
	t.Run("ClaimContinuationOnce", func(t *testing.T) { testClaimContinuationOnce(t, newStore) })
	t.Run("ClaimContinuationReclaimable", func(t *testing.T) { testClaimContinuationReclaimable(t, newStore) })
	t.Run("ClaimContinuationWithoutLease", func(t *testing.T) { testClaimContinuationWithoutLease(t, newStore) })
	t.Run("HoldLeaseRules", func(t *testing.T) { testHoldLeaseRules(t, newStore) })
	t.Run("ListLapsedAcrossScopes", func(t *testing.T) { testListLapsedAcrossScopes(t, newStore) })
	t.Run("MarkReclaimableLapsed", func(t *testing.T) { testMarkReclaimableLapsed(t, newStore) })
	t.Run("MarkReclaimableHandOff", func(t *testing.T) { testMarkReclaimableHandOff(t, newStore) })
	t.Run("UpdateRunLeaseColumns", func(t *testing.T) { testUpdateRunLeaseColumns(t, newStore) })
	t.Run("Notified", func(t *testing.T) { testContinuableNotified(t, newStore) })
}

func continuationStore(t *testing.T, s store.Store) run.ContinuationStore {
	t.Helper()
	cs, ok := s.(run.ContinuationStore)
	if !ok {
		t.Fatalf("%T does not implement run.ContinuationStore", s)
	}
	return cs
}

// leaseAt truncates to the millisecond, the coarsest precision any backend
// keeps, so a lease read back compares equal to the one written.
func leaseAt(owner string, at time.Time) run.Lease {
	return run.Lease{Owner: owner, ExpiresAt: at.UTC().Truncate(time.Millisecond)}
}

func readiness() run.Readiness {
	return run.Readiness{
		Source: run.ReadyApproval,
		// Odd spacing and key order: a store that parsed and re-encoded the
		// bytes would hand back something else.
		Results:      json.RawMessage(`[{"z": 1,  "a":"ok"}]`),
		CheckpointID: id.NewCheckpointID(),
		At:           time.Now().UTC().Truncate(time.Millisecond),
	}
}

// mustRunningRun creates a run under ctx that is already running, holding
// lease when one is given. CreateRun is the write path that carries it.
func mustRunningRun(t *testing.T, s store.Store, ctx context.Context, lease *run.Lease) *run.Run { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	r := newRun(mustCreateAgent(t, s, ctx, ""))
	r.State = run.StateRunning
	if lease != nil {
		held := *lease
		r.Lease = &held
	}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("fixture: create running run: %v", err)
	}
	return r
}

// mustReadyRun suspends a run and marks it ready.
func mustReadyRun(t *testing.T, s store.Store, ctx context.Context) *run.Run { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	r, _ := mustSuspendRun(t, s, ctx, futureDeadline())
	ready, err := continuationStore(t, s).MarkReady(ctx, r.ID, readiness())
	if err != nil {
		t.Fatalf("fixture: mark ready: %v", err)
	}
	return ready
}

// mustReclaimableRun creates a running run whose lease already lapsed and
// lets the sweeper's write move it to reclaimable.
func mustReclaimableRun(t *testing.T, s store.Store, ctx context.Context) *run.Run { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	lapsed := leaseAt("w-lapsed", time.Now().Add(-time.Minute))
	r := mustRunningRun(t, s, ctx, &lapsed)
	reclaimed, err := continuationStore(t, s).MarkReclaimable(ctx, r.ID, time.Now().UTC(), "")
	if err != nil {
		t.Fatalf("fixture: mark reclaimable: %v", err)
	}
	return reclaimed
}

func mustGetRun(t *testing.T, s store.Store, ctx context.Context, runID id.AgentRunID) *run.Run { //nolint:revive // t leads every test helper in this file; ctx after s matches that convention
	t.Helper()
	got, err := s.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	return got
}

func sameReadiness(got *run.Readiness, want run.Readiness) bool {
	return got != nil && got.Source == want.Source && bytes.Equal(got.Results, want.Results) && got.CheckpointID == want.CheckpointID && got.At.Equal(want.At)
}

func sameLease(got *run.Lease, want run.Lease) bool {
	return got != nil && got.Owner == want.Owner && got.ExpiresAt.Equal(want.ExpiresAt)
}

func containsRun(runs []*run.Run, want id.AgentRunID) bool {
	for _, r := range runs {
		if r.ID == want {
			return true
		}
	}
	return false
}

func runIndex(runs []*run.Run, want id.AgentRunID) int {
	for i, r := range runs {
		if r.ID == want {
			return i
		}
	}
	return -1
}

func testMarkReadyFromPaused(t *testing.T, newStore func(t *testing.T) store.Store) {
	for _, prepared := range []bool{false, true} {
		name := "paused"
		if prepared {
			name = "paused_prepared"
		}
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			cs := continuationStore(t, s)
			ctx := ctxWithScope("ready", name)
			r := mustCreateRun(t, s, ctx)
			r.State = run.StatePaused
			if prepared {
				r.Provenance.PreparedDigest = "prepared-digest"
			}
			if err := s.UpdateRun(ctx, r); err != nil {
				t.Fatalf("pause run: %v", err)
			}
			susp := newSuspension(r.ID)
			susp.ExpiresAt = futureDeadline()
			if err := s.CreateSuspension(ctx, susp); err != nil {
				t.Fatalf("create suspension: %v", err)
			}

			want := readiness()
			got, err := cs.MarkReady(ctx, r.ID, want)
			if err != nil {
				t.Fatalf("MarkReady: %v", err)
			}
			if got.State != run.StateReady || got.Revision != r.Revision+1 || !sameReadiness(got.Ready, want) {
				t.Fatalf("MarkReady returned state %q revision %d ready %+v; want ready, revision %d, %+v", got.State, got.Revision, got.Ready, r.Revision+1, want)
			}
			stored := mustGetRun(t, s, ctx, r.ID)
			if stored.State != run.StateReady || stored.Revision != r.Revision+1 || !sameReadiness(stored.Ready, want) {
				t.Fatalf("stored run state %q revision %d ready %+v; want ready, revision %d, %+v", stored.State, stored.Revision, stored.Ready, r.Revision+1, want)
			}
			if prepared && stored.Provenance.PreparedDigest != "prepared-digest" {
				t.Errorf("prepared digest = %q after MarkReady", stored.Provenance.PreparedDigest)
			}
			kept, err := s.GetSuspension(ctx, r.ID)
			if err != nil {
				t.Fatalf("GetSuspension: %v", err)
			}
			if kept.ExpiresAt != nil {
				t.Errorf("suspension ExpiresAt = %v after MarkReady, want nil: a ready run is not the expiry sweeper's", kept.ExpiresAt)
			}
		})
	}
}

func testMarkReadyRefusesOtherStates(t *testing.T, newStore func(t *testing.T) store.Store) {
	for _, state := range []run.State{run.StateRunning, run.StateReady, run.StateCompleted} {
		t.Run(string(state), func(t *testing.T) {
			s := newStore(t)
			cs := continuationStore(t, s)
			ctx := ctxWithScope("ready-refuse", string(state))
			r, _ := mustSuspendRun(t, s, ctx, futureDeadline())
			if state == run.StateReady {
				ready, err := cs.MarkReady(ctx, r.ID, readiness())
				if err != nil {
					t.Fatalf("first MarkReady: %v", err)
				}
				r = ready
			} else {
				r.State = state
				if err := s.UpdateRun(ctx, r); err != nil {
					t.Fatalf("move run to %s: %v", state, err)
				}
			}
			before := mustGetRun(t, s, ctx, r.ID)

			if _, err := cs.MarkReady(ctx, r.ID, readiness()); !errors.Is(err, cortex.ErrNotSuspended) {
				t.Fatalf("MarkReady on a %s run = %v, want ErrNotSuspended", state, err)
			}
			after := mustGetRun(t, s, ctx, r.ID)
			if after.State != before.State || after.Revision != before.Revision || !after.UpdatedAt.Equal(before.UpdatedAt) {
				t.Errorf("refused MarkReady changed the run: before %s/%d, after %s/%d", before.State, before.Revision, after.State, after.Revision)
			}
			if state == run.StateReady && !sameReadiness(after.Ready, *before.Ready) {
				t.Errorf("refused MarkReady replaced the recorded readiness: %+v", after.Ready)
			}
		})
	}
}

func testMarkReadyRefusesExpired(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	cs := continuationStore(t, s)
	ctx := ctxWithScope("ready-expired")
	r, _ := mustSuspendRun(t, s, ctx, pastDeadline())

	if _, err := cs.MarkReady(ctx, r.ID, readiness()); !errors.Is(err, cortex.ErrSuspensionExpired) {
		t.Fatalf("MarkReady past the deadline = %v, want ErrSuspensionExpired", err)
	}
	after := mustGetRun(t, s, ctx, r.ID)
	if after.State != run.StatePaused || after.Revision != r.Revision || after.Ready != nil {
		t.Errorf("expired MarkReady left state %q revision %d ready %+v; want paused, revision %d, no readiness", after.State, after.Revision, after.Ready, r.Revision)
	}
	kept, err := s.GetSuspension(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetSuspension: %v", err)
	}
	if kept.ExpiresAt == nil {
		t.Error("expired MarkReady cleared the deadline the expiry sweeper needs")
	}
}

func testMarkReadyExactScope(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	cs := continuationStore(t, s)
	ctx := ctxWithScope("ws_x", "p1")
	covering := ctxWithScope("ws_x")
	r, _ := mustSuspendRun(t, s, ctx, futureDeadline())

	_, claimErr := s.ClaimSuspension(covering, r.ID)
	if claimErr == nil {
		t.Fatal("a covering scope claimed the suspension")
	}
	_, markErr := cs.MarkReady(covering, r.ID, readiness())
	if markErr == nil {
		t.Fatal("a covering scope marked the run ready")
	}
	matched := false
	for _, sentinel := range []error{cortex.ErrNotSuspended, cortex.ErrSuspensionExpired, cortex.ErrRunNotFound, cortex.ErrInvalidScope} {
		if errors.Is(claimErr, sentinel) {
			matched = true
			if !errors.Is(markErr, sentinel) {
				t.Errorf("MarkReady from a covering scope = %v, ClaimSuspension gives %v", markErr, claimErr)
			}
		}
	}
	if !matched {
		t.Fatalf("ClaimSuspension from a covering scope = %v, which no sentinel here names", claimErr)
	}
	after := mustGetRun(t, s, ctx, r.ID)
	if after.State != run.StatePaused || after.Revision != r.Revision {
		t.Fatalf("covering-scope MarkReady changed the run to %s/%d", after.State, after.Revision)
	}
	if _, err := cs.MarkReady(ctx, r.ID, readiness()); err != nil {
		t.Fatalf("MarkReady from the run's own scope after the refusal: %v", err)
	}
}

func testClaimContinuationOnce(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	cs := continuationStore(t, s)
	ctx := ctxWithScope("claim-once")
	ready := mustReadyRun(t, s, ctx)
	if err := cs.MarkNotified(ctx, ready.ID, time.Now().UTC()); err != nil {
		t.Fatalf("MarkNotified: %v", err)
	}

	leases := []run.Lease{leaseAt("w1", time.Now().Add(time.Minute)), leaseAt("w2", time.Now().Add(time.Minute))}
	claims := make([]*run.Run, len(leases))
	errs := make([]error, len(leases))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range leases {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			claims[i], errs[i] = cs.ClaimContinuation(ctx, ready.ID, leases[i])
		}(i)
	}
	close(start)
	wg.Wait()

	winner := -1
	for i := range leases {
		switch {
		case errs[i] == nil:
			if winner >= 0 {
				t.Fatalf("both ClaimContinuation calls won: %+v and %+v", claims[winner], claims[i])
			}
			winner = i
		case !errors.Is(errs[i], cortex.ErrRunNotContinuable):
			t.Fatalf("losing ClaimContinuation = %v, want ErrRunNotContinuable", errs[i])
		}
	}
	if winner < 0 {
		t.Fatalf("no ClaimContinuation won: %v", errs)
	}
	got := claims[winner]
	if got.State != run.StateRunning || !sameLease(got.Lease, leases[winner]) || got.Ready != nil || got.NotifiedAt != nil || got.Revision != ready.Revision+1 {
		t.Fatalf("winning claim returned state %q lease %+v ready %+v notified %v revision %d", got.State, got.Lease, got.Ready, got.NotifiedAt, got.Revision)
	}
	stored := mustGetRun(t, s, ctx, ready.ID)
	if stored.State != run.StateRunning || !sameLease(stored.Lease, leases[winner]) || stored.Ready != nil || stored.NotifiedAt != nil || stored.Revision != ready.Revision+1 {
		t.Fatalf("stored claim state %q lease %+v ready %+v notified %v revision %d", stored.State, stored.Lease, stored.Ready, stored.NotifiedAt, stored.Revision)
	}
}

func testClaimContinuationReclaimable(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	cs := continuationStore(t, s)
	ctx := ctxWithScope("claim-reclaimable")
	reclaimable := mustReclaimableRun(t, s, ctx)
	lease := leaseAt("w2", time.Now().Add(time.Minute))

	got, err := cs.ClaimContinuation(ctx, reclaimable.ID, lease)
	if err != nil {
		t.Fatalf("ClaimContinuation on a reclaimable run: %v", err)
	}
	if got.State != run.StateRunning || !sameLease(got.Lease, lease) || got.Revision != reclaimable.Revision+1 || got.Reclaims != 1 {
		t.Fatalf("claim returned state %q lease %+v revision %d reclaims %d", got.State, got.Lease, got.Revision, got.Reclaims)
	}
	stored := mustGetRun(t, s, ctx, reclaimable.ID)
	if stored.State != run.StateRunning || !sameLease(stored.Lease, lease) || stored.Reclaims != 1 {
		t.Fatalf("stored claim state %q lease %+v reclaims %d", stored.State, stored.Lease, stored.Reclaims)
	}

	if _, err = cs.ClaimContinuation(ctx, reclaimable.ID, lease); !errors.Is(err, cortex.ErrRunNotContinuable) {
		t.Errorf("ClaimContinuation on a running run = %v, want ErrRunNotContinuable", err)
	}
	paused, _ := mustSuspendRun(t, s, ctx, futureDeadline())
	if _, err = cs.ClaimContinuation(ctx, paused.ID, lease); !errors.Is(err, cortex.ErrRunNotContinuable) {
		t.Errorf("ClaimContinuation on a paused run = %v, want ErrRunNotContinuable", err)
	}
}

// testClaimContinuationWithoutLease pins what a claim with no lease leaves
// behind. Until workers hold leases, a continued run must not look lapsed,
// or the sweeper would reclaim it while it runs.
func testClaimContinuationWithoutLease(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	cs := continuationStore(t, s)
	ctx := ctxWithScope("claim-no-lease")
	ready := mustReadyRun(t, s, ctx)

	got, err := cs.ClaimContinuation(ctx, ready.ID, run.Lease{})
	if err != nil {
		t.Fatalf("ClaimContinuation with no lease: %v", err)
	}
	if got.State != run.StateRunning || got.Lease != nil {
		t.Fatalf("claim with no lease returned state %q lease %+v, want running with no lease", got.State, got.Lease)
	}
	if stored := mustGetRun(t, s, ctx, ready.ID); stored.Lease != nil {
		t.Fatalf("stored run carries lease %+v after a claim with none", stored.Lease)
	}
	lapsed, err := cs.ListLapsedAcrossScopes(context.Background(), time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatalf("ListLapsedAcrossScopes: %v", err)
	}
	if containsRun(lapsed, ready.ID) {
		t.Fatal("a run continued with no lease is listed as lapsed")
	}
	if _, err = cs.MarkReclaimable(ctx, ready.ID, time.Now().Add(time.Hour), ""); !errors.Is(err, cortex.ErrRunLeased) {
		t.Fatalf("MarkReclaimable on a run with no lease = %v, want ErrRunLeased", err)
	}
}

func testHoldLeaseRules(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	cs := continuationStore(t, s)
	ctx := ctxWithScope("ws_hold", "p1")
	r := mustRunningRun(t, s, ctx, nil)
	revision := r.Revision
	checkRevision := func(step string) {
		t.Helper()
		if got := mustGetRun(t, s, ctx, r.ID); got.Revision != revision {
			t.Fatalf("%s: revision = %d, want %d (HoldLease never advances it)", step, got.Revision, revision)
		}
	}

	first := leaseAt("w1", time.Now().Add(time.Minute))
	if err := cs.HoldLease(ctx, r.ID, first); err != nil {
		t.Fatalf("HoldLease on an unowned running run: %v", err)
	}
	if got := mustGetRun(t, s, ctx, r.ID); !sameLease(got.Lease, first) {
		t.Fatalf("lease after acquire = %+v, want %+v", got.Lease, first)
	}
	checkRevision("acquire")

	extended := leaseAt("w1", time.Now().Add(2*time.Minute))
	if err := cs.HoldLease(ctx, r.ID, extended); err != nil {
		t.Fatalf("HoldLease extending its own lease: %v", err)
	}
	if got := mustGetRun(t, s, ctx, r.ID); !sameLease(got.Lease, extended) {
		t.Fatalf("lease after extend = %+v, want %+v", got.Lease, extended)
	}
	checkRevision("extend")

	if err := cs.HoldLease(ctx, r.ID, leaseAt("w2", time.Now().Add(3*time.Minute))); !errors.Is(err, cortex.ErrRunLeaseLost) {
		t.Fatalf("HoldLease by another owner = %v, want ErrRunLeaseLost", err)
	}
	if err := cs.HoldLease(ctxWithScope("ws_hold"), r.ID, extended); !errors.Is(err, cortex.ErrRunLeaseLost) {
		t.Fatalf("HoldLease from a covering scope = %v, want ErrRunLeaseLost", err)
	}
	if err := cs.HoldLease(ctx, r.ID, run.Lease{ExpiresAt: extended.ExpiresAt}); !errors.Is(err, cortex.ErrRunLeaseLost) {
		t.Fatalf("HoldLease with no owner = %v, want ErrRunLeaseLost", err)
	}
	if got := mustGetRun(t, s, ctx, r.ID); !sameLease(got.Lease, extended) {
		t.Fatalf("refused HoldLease changed the lease to %+v", got.Lease)
	}
	checkRevision("refusals")

	paused, _ := mustSuspendRun(t, s, ctx, futureDeadline())
	if err := cs.HoldLease(ctx, paused.ID, first); !errors.Is(err, cortex.ErrRunLeaseLost) {
		t.Fatalf("HoldLease on a paused run = %v, want ErrRunLeaseLost", err)
	}
	if got := mustGetRun(t, s, ctx, paused.ID); got.Lease != nil || got.Revision != paused.Revision {
		t.Fatalf("HoldLease on a paused run left lease %+v revision %d", got.Lease, got.Revision)
	}
}

func testListLapsedAcrossScopes(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	cs := continuationStore(t, s)
	ctxA := ctxWithScope("lapsed-a")
	ctxB := ctxWithScope("lapsed-b", "p1")
	older := leaseAt("wa", time.Now().Add(-2*time.Minute))
	newer := leaseAt("wb", time.Now().Add(-time.Minute))
	a := mustRunningRun(t, s, ctxA, &older)
	b := mustRunningRun(t, s, ctxB, &newer)
	unleased := mustRunningRun(t, s, ctxA, nil)
	live := leaseAt("wl", time.Now().Add(time.Hour))
	held := mustRunningRun(t, s, ctxB, &live)

	listed, err := cs.ListLapsedAcrossScopes(context.Background(), time.Now().UTC(), 0)
	if err != nil {
		t.Fatalf("ListLapsedAcrossScopes: %v", err)
	}
	ia, ib := runIndex(listed, a.ID), runIndex(listed, b.ID)
	if ia < 0 || ib < 0 {
		t.Fatalf("lapsed runs in two scopes were not both listed: %d runs, a at %d, b at %d", len(listed), ia, ib)
	}
	if ia > ib {
		t.Errorf("lapsed runs out of order: the older expiry is at %d, the newer at %d", ia, ib)
	}
	if containsRun(listed, unleased.ID) || containsRun(listed, held.ID) {
		t.Error("an unleased run or a live lease was listed as lapsed")
	}
	for _, r := range listed {
		if r.ID == a.ID && (!sameLease(r.Lease, older) || r.Scope.Canonical() != scopeOf("lapsed-a").Canonical()) {
			t.Errorf("listed run a has lease %+v scope %q", r.Lease, r.Scope.Canonical())
		}
	}

	limited, err := cs.ListLapsedAcrossScopes(context.Background(), time.Now().UTC(), 1)
	if err != nil || len(limited) != 1 || limited[0].ID != a.ID {
		t.Fatalf("ListLapsedAcrossScopes with limit 1 = %d runs, %v; want the oldest expiry only", len(limited), err)
	}
}

func testMarkReclaimableLapsed(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	cs := continuationStore(t, s)
	ctx := ctxWithScope("reclaim-lapsed")
	lapsed := leaseAt("w1", time.Now().Add(-time.Minute))
	r := mustRunningRun(t, s, ctx, &lapsed)

	got, err := cs.MarkReclaimable(ctx, r.ID, time.Now().UTC(), "")
	if err != nil {
		t.Fatalf("MarkReclaimable on a lapsed lease: %v", err)
	}
	if got.State != run.StateReclaimable || got.Reclaims != 1 || got.Lease != nil || got.Revision != r.Revision+1 {
		t.Fatalf("MarkReclaimable returned state %q reclaims %d lease %+v revision %d", got.State, got.Reclaims, got.Lease, got.Revision)
	}
	stored := mustGetRun(t, s, ctx, r.ID)
	if stored.State != run.StateReclaimable || stored.Reclaims != 1 || stored.Lease != nil || stored.Revision != r.Revision+1 {
		t.Fatalf("stored run state %q reclaims %d lease %+v revision %d", stored.State, stored.Reclaims, stored.Lease, stored.Revision)
	}

	live := leaseAt("w2", time.Now().Add(time.Hour))
	held := mustRunningRun(t, s, ctx, &live)
	if _, err = cs.MarkReclaimable(ctx, held.ID, time.Now().UTC(), ""); !errors.Is(err, cortex.ErrRunLeased) {
		t.Fatalf("MarkReclaimable on a live lease = %v, want ErrRunLeased", err)
	}
	if after := mustGetRun(t, s, ctx, held.ID); after.State != run.StateRunning || !sameLease(after.Lease, live) || after.Revision != held.Revision {
		t.Fatalf("refused MarkReclaimable changed the run: state %q lease %+v revision %d", after.State, after.Lease, after.Revision)
	}

	if _, err = cs.MarkReclaimable(ctx, r.ID, time.Now().UTC(), ""); !errors.Is(err, cortex.ErrRunConflict) {
		t.Fatalf("MarkReclaimable on a run that is no longer running = %v, want ErrRunConflict", err)
	}
}

func testMarkReclaimableHandOff(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	cs := continuationStore(t, s)
	ctx := ctxWithScope("reclaim-handoff")
	live := leaseAt("w1", time.Now().Add(time.Hour))
	r := mustRunningRun(t, s, ctx, &live)

	if _, err := cs.MarkReclaimable(ctx, r.ID, time.Now().UTC(), "w2"); !errors.Is(err, cortex.ErrRunLeaseLost) {
		t.Fatalf("hand-off by an owner that does not hold the lease = %v, want ErrRunLeaseLost", err)
	}
	if after := mustGetRun(t, s, ctx, r.ID); after.State != run.StateRunning || !sameLease(after.Lease, live) || after.Revision != r.Revision {
		t.Fatalf("refused hand-off changed the run: state %q lease %+v revision %d", after.State, after.Lease, after.Revision)
	}

	got, err := cs.MarkReclaimable(ctx, r.ID, time.Now().UTC(), "w1")
	if err != nil {
		t.Fatalf("hand-off by the lease owner: %v", err)
	}
	if got.State != run.StateReclaimable || got.Reclaims != 0 || got.Lease != nil || got.Revision != r.Revision+1 {
		t.Fatalf("hand-off returned state %q reclaims %d lease %+v revision %d", got.State, got.Reclaims, got.Lease, got.Revision)
	}
	if stored := mustGetRun(t, s, ctx, r.ID); stored.State != run.StateReclaimable || stored.Reclaims != 0 || stored.Lease != nil {
		t.Fatalf("stored hand-off state %q reclaims %d lease %+v", stored.State, stored.Reclaims, stored.Lease)
	}
}

func testUpdateRunLeaseColumns(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	cs := continuationStore(t, s)
	ctx := ctxWithScope("update-lease")
	first := leaseAt("w1", time.Now().Add(time.Minute))
	r := mustRunningRun(t, s, ctx, &first)
	if got := mustGetRun(t, s, ctx, r.ID); !sameLease(got.Lease, first) {
		t.Fatalf("CreateRun stored lease %+v, want %+v", got.Lease, first)
	}

	extended := leaseAt("w1", time.Now().Add(time.Hour))
	if err := cs.HoldLease(ctx, r.ID, extended); err != nil {
		t.Fatalf("HoldLease: %v", err)
	}
	// r still carries the lease it was created with. A step write from the
	// worker must not wind the heartbeat back to it.
	r.StepCount = 1
	if err := s.UpdateRun(ctx, r); err != nil {
		t.Fatalf("UpdateRun keeping running: %v", err)
	}
	if got := mustGetRun(t, s, ctx, r.ID); !sameLease(got.Lease, extended) || got.StepCount != 1 {
		t.Fatalf("after UpdateRun keeping running: lease %+v step count %d; want %+v and 1", got.Lease, got.StepCount, extended)
	}

	r.State = run.StateCompleted
	if err := s.UpdateRun(ctx, r); err != nil {
		t.Fatalf("UpdateRun to completed: %v", err)
	}
	if got := mustGetRun(t, s, ctx, r.ID); got.Lease != nil || got.State != run.StateCompleted {
		t.Fatalf("after UpdateRun to completed: state %q lease %+v, want no lease", got.State, got.Lease)
	}
	lapsed, err := cs.ListLapsedAcrossScopes(context.Background(), time.Now().Add(2*time.Hour), 0)
	if err != nil {
		t.Fatalf("ListLapsedAcrossScopes: %v", err)
	}
	if containsRun(lapsed, r.ID) {
		t.Fatal("a completed run is listed as lapsed")
	}
}

func testContinuableNotified(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	cs := continuationStore(t, s)
	ctxA := ctxWithScope("notified-a")
	ctxB := ctxWithScope("notified-b", "p1")
	ready := mustReadyRun(t, s, ctxA)
	reclaimable := mustReclaimableRun(t, s, ctxB)
	running := mustRunningRun(t, s, ctxA, nil)
	paused, _ := mustSuspendRun(t, s, ctxB, futureDeadline())

	before := time.Now().Add(time.Minute)
	listed, err := cs.ListContinuableAcrossScopes(context.Background(), before, 0)
	if err != nil {
		t.Fatalf("ListContinuableAcrossScopes: %v", err)
	}
	iReady, iReclaimable := runIndex(listed, ready.ID), runIndex(listed, reclaimable.ID)
	if iReady < 0 || iReclaimable < 0 {
		t.Fatalf("continuable runs not both listed: ready at %d, reclaimable at %d of %d", iReady, iReclaimable, len(listed))
	}
	if iReady > iReclaimable {
		t.Errorf("continuable runs out of order: the older update is at %d, the newer at %d", iReady, iReclaimable)
	}
	if containsRun(listed, running.ID) || containsRun(listed, paused.ID) {
		t.Error("a running or paused run was listed as continuable")
	}
	for _, r := range listed {
		if r.ID == ready.ID && (!sameReadiness(r.Ready, *ready.Ready) || r.Scope.Canonical() != scopeOf("notified-a").Canonical()) {
			t.Errorf("listed ready run has readiness %+v scope %q", r.Ready, r.Scope.Canonical())
		}
	}

	at := time.Now().UTC().Truncate(time.Millisecond)
	if err = cs.MarkNotified(ctxA, ready.ID, at); err != nil {
		t.Fatalf("MarkNotified: %v", err)
	}
	stored := mustGetRun(t, s, ctxA, ready.ID)
	if stored.NotifiedAt == nil || !stored.NotifiedAt.Equal(at) || stored.Revision != ready.Revision || stored.State != run.StateReady {
		t.Fatalf("after MarkNotified: notified %v revision %d state %q; want %v, revision %d, ready", stored.NotifiedAt, stored.Revision, stored.State, at, ready.Revision)
	}
	hidden, err := cs.ListContinuableAcrossScopes(context.Background(), at, 0)
	if err != nil {
		t.Fatalf("ListContinuableAcrossScopes after MarkNotified: %v", err)
	}
	if containsRun(hidden, ready.ID) || !containsRun(hidden, reclaimable.ID) {
		t.Fatalf("this period's listing: ready listed %v, reclaimable listed %v; want only the reclaimable run", containsRun(hidden, ready.ID), containsRun(hidden, reclaimable.ID))
	}
	next, err := cs.ListContinuableAcrossScopes(context.Background(), at.Add(time.Second), 0)
	if err != nil {
		t.Fatalf("ListContinuableAcrossScopes next period: %v", err)
	}
	if !containsRun(next, ready.ID) {
		t.Fatal("a run notified last period is not listed in the next one")
	}
	limited, err := cs.ListContinuableAcrossScopes(context.Background(), before, 1)
	if err != nil || len(limited) != 1 {
		t.Fatalf("ListContinuableAcrossScopes with limit 1 = %d runs, %v", len(limited), err)
	}

	if err = cs.MarkNotified(ctxA, running.ID, at); !errors.Is(err, cortex.ErrRunNotContinuable) {
		t.Fatalf("MarkNotified on a running run = %v, want ErrRunNotContinuable", err)
	}
	if err = cs.MarkNotified(ctxWithScope("notified-b"), reclaimable.ID, at); !errors.Is(err, cortex.ErrRunNotContinuable) {
		t.Fatalf("MarkNotified from a covering scope = %v, want ErrRunNotContinuable", err)
	}
	if got := mustGetRun(t, s, ctxB, reclaimable.ID); got.NotifiedAt != nil || got.Revision != reclaimable.Revision {
		t.Fatalf("refused MarkNotified left notified %v revision %d", got.NotifiedAt, got.Revision)
	}
}
