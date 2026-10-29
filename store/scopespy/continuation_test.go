package scopespy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

// The spy's continuation methods follow the real stores' rules closely
// enough that an engine test against it cannot pass on a transition a
// real store would refuse.
func TestSpy_ContinuationFollowsStoreRules(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}})
	spy := New()
	r := &run.Run{ID: id.NewAgentRunID(), State: run.StatePaused}
	if err := spy.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Hour)
	suspID := id.NewSuspensionID()
	if err := spy.CreateSuspension(ctx, &suspension.Suspension{ID: suspID, RunID: r.ID, ExpiresAt: &deadline}); err != nil {
		t.Fatal(err)
	}
	if _, err := spy.MarkReady(ctx, r.ID, run.Readiness{Source: run.ReadyApproval, SuspensionID: id.NewSuspensionID()}); !errors.Is(err, cortex.ErrNotSuspended) {
		t.Fatalf("MarkReady naming another pause = %v, want ErrNotSuspended", err)
	}
	ready, err := spy.MarkReady(ctx, r.ID, run.Readiness{Source: run.ReadyApproval, SuspensionID: suspID})
	if err != nil || ready.State != run.StateReady || spy.Suspensions()[0].ExpiresAt != nil {
		t.Fatalf("MarkReady = %+v, %v; want ready with the deadline cleared", ready, err)
	}
	if _, err = spy.MarkReady(ctx, r.ID, run.Readiness{}); !errors.Is(err, cortex.ErrNotSuspended) {
		t.Fatalf("second MarkReady = %v, want ErrNotSuspended", err)
	}
	lease := run.Lease{Owner: "w1", ExpiresAt: time.Now().Add(-time.Second)}
	claimed, err := spy.ClaimContinuation(ctx, r.ID, lease)
	if err != nil || claimed.State != run.StateRunning || claimed.Lease == nil || claimed.Ready != nil {
		t.Fatalf("ClaimContinuation = %+v, %v", claimed, err)
	}
	if _, err = spy.ClaimContinuation(ctx, r.ID, lease); !errors.Is(err, cortex.ErrRunNotContinuable) {
		t.Fatalf("second ClaimContinuation = %v, want ErrRunNotContinuable", err)
	}
	if err = spy.HoldLease(ctx, r.ID, run.Lease{Owner: "w2", ExpiresAt: time.Now().Add(time.Minute)}); !errors.Is(err, cortex.ErrRunLeaseLost) {
		t.Fatalf("HoldLease by another owner = %v, want ErrRunLeaseLost", err)
	}
	lapsed, err := spy.ListLapsedAcrossScopes(context.Background(), time.Now(), 0)
	if err != nil || len(lapsed) != 1 {
		t.Fatalf("ListLapsedAcrossScopes = %d runs, %v", len(lapsed), err)
	}
	reclaimed, err := spy.MarkReclaimable(ctx, r.ID, time.Now(), "")
	if err != nil || reclaimed.State != run.StateReclaimable || reclaimed.Reclaims != 1 || reclaimed.Lease != nil {
		t.Fatalf("MarkReclaimable = %+v, %v", reclaimed, err)
	}
	if err = spy.MarkNotified(ctx, r.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	waiting, err := spy.ListContinuableAcrossScopes(context.Background(), time.Now().Add(-time.Minute), 0)
	if err != nil || len(waiting) != 0 {
		t.Fatalf("a run notified this period is listed again: %d runs, %v", len(waiting), err)
	}
}

// A running run nobody holds is taken only once it has been idle past the
// sweeper's grace, and never when coordination owns it.
func TestSpy_OwnerlessFollowsStoreRules(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}})
	spy := New()
	idle := time.Now().Add(-time.Hour)
	ownerless := &run.Run{ID: id.NewAgentRunID(), State: run.StateRunning}
	ownerless.UpdatedAt = idle
	claimed := &run.Run{ID: id.NewAgentRunID(), State: run.StateRunning}
	claimed.UpdatedAt = idle
	claimed.Provenance.TaskClaimID = "claim"
	leased := &run.Run{ID: id.NewAgentRunID(), State: run.StateRunning, Lease: &run.Lease{Owner: "w1", ExpiresAt: time.Now().Add(time.Minute)}}
	leased.UpdatedAt = idle
	for _, r := range []*run.Run{ownerless, claimed, leased} {
		if err := spy.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := spy.ListOwnerlessAcrossScopes(context.Background(), time.Now(), 0)
	if err != nil || len(listed) != 1 || listed[0].ID != ownerless.ID {
		t.Fatalf("ListOwnerlessAcrossScopes = %v, %v; want only the ownerless run", listed, err)
	}
	if _, err = spy.MarkOwnerlessReclaimable(ctx, ownerless.ID, idle.Add(-time.Minute)); !errors.Is(err, cortex.ErrRunLeased) {
		t.Fatalf("MarkOwnerlessReclaimable inside the grace = %v, want ErrRunLeased", err)
	}
	for _, r := range []*run.Run{claimed, leased} {
		if _, err = spy.MarkOwnerlessReclaimable(ctx, r.ID, time.Now()); !errors.Is(err, cortex.ErrRunLeased) {
			t.Fatalf("MarkOwnerlessReclaimable(%s) = %v, want ErrRunLeased", r.ID, err)
		}
	}
	moved, err := spy.MarkOwnerlessReclaimable(ctx, ownerless.ID, time.Now())
	if err != nil || moved.State != run.StateReclaimable || moved.Reclaims != 1 {
		t.Fatalf("MarkOwnerlessReclaimable = %+v, %v", moved, err)
	}
	if _, err = spy.MarkOwnerlessReclaimable(ctx, ownerless.ID, time.Now()); !errors.Is(err, cortex.ErrRunConflict) {
		t.Fatalf("second MarkOwnerlessReclaimable = %v, want ErrRunConflict", err)
	}
}
