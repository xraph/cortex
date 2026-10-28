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
	if err := spy.CreateSuspension(ctx, &suspension.Suspension{ID: id.NewSuspensionID(), RunID: r.ID, ExpiresAt: &deadline}); err != nil {
		t.Fatal(err)
	}
	ready, err := spy.MarkReady(ctx, r.ID, run.Readiness{Source: run.ReadyApproval})
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
