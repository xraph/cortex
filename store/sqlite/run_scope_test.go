package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

// scopeOf builds a cortex.Scope from positional level values, using the
// same workspace/project/environment key ordering as
// store/postgres/scope_test.go's ws() helper.
func scopeOf(vals ...string) cortex.Scope {
	keys := []string{"workspace", "project", "environment"}
	s := cortex.Scope{}
	for i, v := range vals {
		s.Levels = append(s.Levels, cortex.Level{Key: keys[i], Value: v})
	}
	return s
}

func newTestRun(agentID id.AgentID) *run.Run {
	return &run.Run{
		ID:      id.NewAgentRunID(),
		AgentID: agentID,
		State:   run.StateCreated,
		Input:   "original",
	}
}

// TestRunStore_ZeroScopeRejected covers every run.Store method against a
// context that carries no scope: each must return cortex.ErrNoScope
// without touching the database, rather than silently querying/writing
// across every tenant.
func TestRunStore_ZeroScopeRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background() // deliberately no scope
	agentID := id.NewAgentID()
	r := newTestRun(agentID)

	if err := s.CreateRun(ctx, r); !errors.Is(err, cortex.ErrNoScope) {
		t.Errorf("CreateRun with no scope = %v, want ErrNoScope", err)
	}
	if _, err := s.GetRun(ctx, r.ID); !errors.Is(err, cortex.ErrNoScope) {
		t.Errorf("GetRun with no scope = %v, want ErrNoScope", err)
	}
	if err := s.UpdateRun(ctx, r); !errors.Is(err, cortex.ErrNoScope) {
		t.Errorf("UpdateRun with no scope = %v, want ErrNoScope", err)
	}
	if _, err := s.ListRuns(ctx, nil); !errors.Is(err, cortex.ErrNoScope) {
		t.Errorf("ListRuns with no scope = %v, want ErrNoScope", err)
	}
	if _, err := s.CountRuns(ctx, nil); !errors.Is(err, cortex.ErrNoScope) {
		t.Errorf("CountRuns with no scope = %v, want ErrNoScope", err)
	}
}

// TestRunStore_CreateWritesNonNullScopeExtra is a regression test for the
// nil-map hazard: grove writes a nil Go map as SQL NULL, which the
// scope_extra NOT NULL column rejects. Before scopeColumns/runToModel
// were fixed to always produce a non-nil map, this Create failed outright
// with a NOT NULL constraint violation.
func TestRunStore_CreateWritesNonNullScopeExtra(t *testing.T) {
	s := newTestStore(t)
	ctx := cortex.WithScope(context.Background(), scopeOf("ws_x", "proj_y"))
	r := newTestRun(id.NewAgentID())

	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("create run: %v", err)
	}

	got, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	const want = "workspace=ws_x/project=proj_y"
	if got.Scope.Canonical() != want {
		t.Errorf("Scope.Canonical() = %q, want %q", got.Scope.Canonical(), want)
	}
}

// TestRunStore_ListPrefixFiltersByScope proves the scope predicate
// actually filters instead of silently matching everything: two runs are
// created under different top-level scopes, and listing from one of those
// scopes must return only the matching run.
func TestRunStore_ListPrefixFiltersByScope(t *testing.T) {
	s := newTestStore(t)
	agentID := id.NewAgentID()

	ctxA := cortex.WithScope(context.Background(), scopeOf("ws_a"))
	ctxB := cortex.WithScope(context.Background(), scopeOf("ws_b"))

	runA := newTestRun(agentID)
	if err := s.CreateRun(ctxA, runA); err != nil {
		t.Fatalf("create run A: %v", err)
	}
	runB := newTestRun(agentID)
	if err := s.CreateRun(ctxB, runB); err != nil {
		t.Fatalf("create run B: %v", err)
	}

	got, err := s.ListRuns(ctxA, nil)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(got) != 1 || got[0].ID != runA.ID {
		t.Fatalf("ListRuns(ctxA) = %d run(s), want exactly runA (predicate isn't filtering)", len(got))
	}

	count, err := s.CountRuns(ctxA, nil)
	if err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountRuns(ctxA) = %d, want 1", count)
	}

	// Sanity check the other side too, so a predicate that always matches
	// nothing wouldn't also pass silently.
	gotB, err := s.ListRuns(ctxB, nil)
	if err != nil {
		t.Fatalf("list runs B: %v", err)
	}
	if len(gotB) != 1 || gotB[0].ID != runB.ID {
		t.Fatalf("ListRuns(ctxB) = %d run(s), want exactly runB", len(gotB))
	}
}

// TestRunStore_UpdateDoesNotMutateScope pins Fix-Round-1 Finding 1: a
// run's scope is immutable after creation. UpdateRun must use the context
// scope only as an authorization predicate, never write it back — even
// when the update comes from a broader (but still matching) scope than
// the one the run was created under.
func TestRunStore_UpdateDoesNotMutateScope(t *testing.T) {
	s := newTestStore(t)
	createCtx := cortex.WithScope(context.Background(), scopeOf("ws_x", "proj_y"))

	r := newTestRun(id.NewAgentID())
	if err := s.CreateRun(createCtx, r); err != nil {
		t.Fatalf("create run: %v", err)
	}

	loaded, err := s.GetRun(createCtx, r.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	loaded.Input = "mutated"

	// Update from a broader context scoped only to workspace=ws_x — no
	// project level. Prefix matching lets this authorize the update (it's
	// a superset of the run's stored scope), but the write must not
	// collapse the row's own scope down to that broader context's.
	updateCtx := cortex.WithScope(context.Background(), scopeOf("ws_x"))
	if updateErr := s.UpdateRun(updateCtx, loaded); updateErr != nil {
		t.Fatalf("update run: %v", updateErr)
	}

	reloaded, err := s.GetRun(createCtx, r.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	const want = "workspace=ws_x/project=proj_y"
	if reloaded.Scope.Canonical() != want {
		t.Errorf("scope after update = %q, want %q (scope must be immutable)", reloaded.Scope.Canonical(), want)
	}
	if reloaded.Input != "mutated" {
		t.Errorf("Input after update = %q, want %q", reloaded.Input, "mutated")
	}
}
