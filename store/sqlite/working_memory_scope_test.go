package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// TestWorkingMemoryStore_ZeroScopeRejected covers every working-memory
// method against a context that carries no scope: each must return
// cortex.ErrNoScope without touching the database, mirroring
// TestCheckpointStore_ZeroScopeRejected. A run ID is a bearer capability,
// not an isolation boundary, so these guards close that gap the same way
// the conversation/summary guards already do.
func TestWorkingMemoryStore_ZeroScopeRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background() // deliberately no scope
	runID := id.NewAgentRunID()

	if err := s.SaveWorking(ctx, runID, "k", "v"); !errors.Is(err, cortex.ErrNoScope) {
		t.Errorf("SaveWorking with no scope = %v, want ErrNoScope", err)
	}
	if _, err := s.LoadWorking(ctx, runID, "k"); !errors.Is(err, cortex.ErrNoScope) {
		t.Errorf("LoadWorking with no scope = %v, want ErrNoScope", err)
	}
	if err := s.ClearWorking(ctx, runID); !errors.Is(err, cortex.ErrNoScope) {
		t.Errorf("ClearWorking with no scope = %v, want ErrNoScope", err)
	}
}

// TestWorkingMemoryStore_CrossScopeWritesDoNotCollide is the regression
// test for the working-memory write-path vulnerability: before
// idx_cortex_memories_working carried scope_canon (migration
// 20260822000002), SaveWorking's ON CONFLICT target was
// (agent_id, kind, key) with no scope component at all. A run ID is a
// bearer capability, not an isolation boundary, so a caller in scope B
// who merely knew scope A's run ID could save under the SAME
// (agent_id, kind, key) and hit A's exact conflict target: the DO
// UPDATE silently overwrote A's content while leaving A's own scope
// columns in place, so A's next scoped LoadWorking returned B's value
// instead of its own.
//
// This only reproduces with the SAME run ID reused under two different
// scopes — an earlier version of this test used two different run IDs,
// which never share a conflict target and would pass regardless of
// whether the index or the ON CONFLICT clause carried scope. That
// version's justifying comment ("namespace collision setup is
// impossible") was wrong: nothing about IDs being globally unique
// prevents the SAME id from being reused deliberately across two scopes,
// which is exactly what a bearer-capability run ID lets happen.
func TestWorkingMemoryStore_CrossScopeWritesDoNotCollide(t *testing.T) {
	s := newTestStore(t)
	runID := id.NewAgentRunID() // one run ID, shared across both scopes below

	ctxA := cortex.WithScope(context.Background(), scopeOf("ws_a"))
	ctxB := cortex.WithScope(context.Background(), scopeOf("ws_b"))

	if err := s.SaveWorking(ctxA, runID, "k", "value-a"); err != nil {
		t.Fatalf("save working A: %v", err)
	}
	// The dangerous write: scope B saves under the exact same run ID and
	// key scope A just used. Before the index/conflict-target fix, this
	// silently overwrote A's row instead of creating a separate one.
	if err := s.SaveWorking(ctxB, runID, "k", "value-b"); err != nil {
		t.Fatalf("save working B: %v", err)
	}

	gotA, err := s.LoadWorking(ctxA, runID, "k")
	if err != nil {
		t.Fatalf("load working A: %v", err)
	}
	if gotA != "value-a" {
		t.Fatalf("LoadWorking(ctxA, runID) = %v, want %q (scope B's write must not clobber scope A's row)", gotA, "value-a")
	}

	gotB, err := s.LoadWorking(ctxB, runID, "k")
	if err != nil {
		t.Fatalf("load working B: %v", err)
	}
	if gotB != "value-b" {
		t.Fatalf("LoadWorking(ctxB, runID) = %v, want %q", gotB, "value-b")
	}

	// Clearing runID's working memory from ctxB must only ever touch
	// B's own row: A's value must still be readable afterward.
	if clearErr := s.ClearWorking(ctxB, runID); clearErr != nil {
		t.Fatalf("clear working (ctxB): %v", clearErr)
	}
	stillThere, err := s.LoadWorking(ctxA, runID, "k")
	if err != nil {
		t.Fatalf("reload working A after ctxB clear: %v", err)
	}
	if stillThere != "value-a" {
		t.Fatalf("value after ctxB's ClearWorking = %v, want %q (must not delete another scope's row)", stillThere, "value-a")
	}

	// Clearing from the correct scope removes that scope's own row.
	if err := s.ClearWorking(ctxA, runID); err != nil {
		t.Fatalf("clear working (ctxA): %v", err)
	}
	if _, err := s.LoadWorking(ctxA, runID, "k"); err == nil {
		t.Error("LoadWorking(ctxA) after same-scope ClearWorking succeeded; want an error (row should be gone)")
	}
}
