package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

// CheckpointSweep proves the sweeper's listing of pending checkpoints:
// every scope, pending only, in id order, resumable after any id.
func CheckpointSweep(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	sweep, ok := s.(checkpoint.SweepStore)
	if !ok {
		t.Fatalf("%T does not implement checkpoint.SweepStore", s)
	}
	ctxA := ctxWithScope("cp-sweep-a")
	ctxB := ctxWithScope("cp-sweep-b", "p1")
	runA := mustCreateRun(t, s, ctxA)
	runB := mustCreateRun(t, s, ctxB)
	first := mustCreateCheckpoint(t, s, ctxA, runA.ID, runA.AgentID)
	second := mustCreateCheckpoint(t, s, ctxB, runB.ID, runB.AgentID)
	decided := mustCreateCheckpoint(t, s, ctxA, runA.ID, runA.AgentID)
	if err := s.Resolve(ctxA, decided.ID, checkpoint.Decision{Approved: true, DecidedBy: "reviewer", DecidedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("fixture: resolve: %v", err)
	}

	listed, err := sweep.ListPendingAcrossScopes(context.Background(), id.CheckpointID{}, 0)
	if err != nil {
		t.Fatalf("ListPendingAcrossScopes: %v", err)
	}
	ids := make([]string, len(listed))
	for i, cp := range listed {
		ids[i] = cp.ID.String()
		if cp.State != "pending" {
			t.Errorf("listed checkpoint %s is %s", cp.ID, cp.State)
		}
		if cp.ID == decided.ID {
			t.Error("a decided checkpoint was listed as pending")
		}
		if cp.ID == second.ID && cp.Scope.Canonical() != scopeOf("cp-sweep-b", "p1").Canonical() {
			t.Errorf("listed checkpoint carries scope %q", cp.Scope.Canonical())
		}
	}
	if len(listed) != 2 {
		t.Fatalf("ListPendingAcrossScopes = %v, want the two pending checkpoints in both scopes", ids)
	}
	if ids[0] > ids[1] {
		t.Errorf("listed out of id order: %v", ids)
	}
	low, high := first.ID, second.ID
	if low.String() > high.String() {
		low, high = high, low
	}
	rest, err := sweep.ListPendingAcrossScopes(context.Background(), low, 0)
	if err != nil || len(rest) != 1 || rest[0].ID != high {
		t.Fatalf("ListPendingAcrossScopes after %s = %d checkpoints, %v; want only %s", low, len(rest), err, high)
	}
	limited, err := sweep.ListPendingAcrossScopes(context.Background(), id.CheckpointID{}, 1)
	if err != nil || len(limited) != 1 || limited[0].ID != low {
		t.Fatalf("ListPendingAcrossScopes with limit 1 = %d checkpoints, %v; want %s", len(limited), err, low)
	}
}
