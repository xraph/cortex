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

// TestWorkingMemoryStore_CrossScopeReadsRefused proves the scope
// predicate on LoadWorking/ClearWorking actually filters instead of
// silently matching everything: two runs happen to share the same run ID
// namespace collision setup is impossible (IDs are unique), so instead
// this pins the real-world hazard directly — two DIFFERENT run IDs each
// save a value under the same key, then a read from one scope must not
// see the other's value, and a clear from one scope must not delete the
// other's row.
func TestWorkingMemoryStore_CrossScopeReadsRefused(t *testing.T) {
	s := newTestStore(t)
	runIDA := id.NewAgentRunID()
	runIDB := id.NewAgentRunID()

	ctxA := cortex.WithScope(context.Background(), scopeOf("ws_a"))
	ctxB := cortex.WithScope(context.Background(), scopeOf("ws_b"))

	if err := s.SaveWorking(ctxA, runIDA, "k", "value-a"); err != nil {
		t.Fatalf("save working A: %v", err)
	}
	if err := s.SaveWorking(ctxB, runIDB, "k", "value-b"); err != nil {
		t.Fatalf("save working B: %v", err)
	}

	gotA, err := s.LoadWorking(ctxA, runIDA, "k")
	if err != nil {
		t.Fatalf("load working A: %v", err)
	}
	if gotA != "value-a" {
		t.Fatalf("LoadWorking(ctxA) = %v, want %q", gotA, "value-a")
	}

	// Loading runIDA's key from ctxB's scope must refuse: wrong scope,
	// even though the run ID itself is correct and known.
	if _, crossErr := s.LoadWorking(ctxB, runIDA, "k"); crossErr == nil {
		t.Error("LoadWorking(ctxB, runIDA) succeeded; want an error (predicate isn't filtering)")
	}

	// Clearing runIDA's working memory from ctxB must be a no-op: the
	// value must still be readable from ctxA afterward.
	if clearErr := s.ClearWorking(ctxB, runIDA); clearErr != nil {
		t.Fatalf("clear working (cross-scope): %v", clearErr)
	}
	stillThere, err := s.LoadWorking(ctxA, runIDA, "k")
	if err != nil {
		t.Fatalf("reload working A after cross-scope clear: %v", err)
	}
	if stillThere != "value-a" {
		t.Fatalf("value after cross-scope ClearWorking = %v, want %q (must not delete)", stillThere, "value-a")
	}

	// Clearing from the correct scope must succeed.
	if err := s.ClearWorking(ctxA, runIDA); err != nil {
		t.Fatalf("clear working (correct scope): %v", err)
	}
	if _, err := s.LoadWorking(ctxA, runIDA, "k"); err == nil {
		t.Error("LoadWorking after same-scope ClearWorking succeeded; want an error (row should be gone)")
	}
}
