package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
)

func newTestCheckpoint(runID id.AgentRunID, agentID id.AgentID) *checkpoint.Checkpoint {
	return &checkpoint.Checkpoint{
		ID:        id.NewCheckpointID(),
		RunID:     runID,
		AgentID:   agentID,
		Reason:    "needs approval",
		StepIndex: 1,
		State:     "pending",
	}
}

// TestCheckpointStore_ZeroScopeRejected covers every checkpoint.Store
// method against a context that carries no scope: each must return
// cortex.ErrNoScope without touching the database, rather than silently
// crossing every scope the way the pre-fix regression did.
func TestCheckpointStore_ZeroScopeRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background() // deliberately no scope
	runID := id.NewAgentRunID()
	agentID := id.NewAgentID()
	cp := newTestCheckpoint(runID, agentID)

	if err := s.CreateCheckpoint(ctx, cp); !errors.Is(err, cortex.ErrNoScope) {
		t.Errorf("CreateCheckpoint with no scope = %v, want ErrNoScope", err)
	}
	if _, err := s.GetCheckpoint(ctx, cp.ID); !errors.Is(err, cortex.ErrNoScope) {
		t.Errorf("GetCheckpoint with no scope = %v, want ErrNoScope", err)
	}
	if err := s.Resolve(ctx, cp.ID, checkpoint.Decision{Approved: true}); !errors.Is(err, cortex.ErrNoScope) {
		t.Errorf("Resolve with no scope = %v, want ErrNoScope", err)
	}
	if _, err := s.ListPending(ctx, nil); !errors.Is(err, cortex.ErrNoScope) {
		t.Errorf("ListPending with no scope = %v, want ErrNoScope", err)
	}
	if _, err := s.CountPending(ctx, nil); !errors.Is(err, cortex.ErrNoScope) {
		t.Errorf("CountPending with no scope = %v, want ErrNoScope", err)
	}
}

// TestCheckpointStore_CreateWritesScope proves scope round-trips through
// CreateCheckpoint into the stored canonical form, mirroring
// TestRunStore_CreateWritesNonNullScopeExtra.
func TestCheckpointStore_CreateWritesScope(t *testing.T) {
	s := newTestStore(t)
	ctx := cortex.WithScope(context.Background(), scopeOf("ws_x", "proj_y"))
	cp := newTestCheckpoint(id.NewAgentRunID(), id.NewAgentID())

	if err := s.CreateCheckpoint(ctx, cp); err != nil {
		t.Fatalf("create checkpoint: %v", err)
	}

	got, err := s.GetCheckpoint(ctx, cp.ID)
	if err != nil {
		t.Fatalf("get checkpoint: %v", err)
	}
	const want = "workspace=ws_x/project=proj_y"
	if got.Scope.Canonical() != want {
		t.Errorf("Scope.Canonical() = %q, want %q", got.Scope.Canonical(), want)
	}
}

// TestCheckpointStore_CrossScopeReadsRefused proves the scope predicate
// actually filters instead of silently matching everything: this is the
// regression test for the phase's Critical finding, where GetCheckpoint,
// Resolve, ListPending and CountPending crossed every scope with no
// filter after ListFilter.TenantID was removed and nothing replaced it.
func TestCheckpointStore_CrossScopeReadsRefused(t *testing.T) {
	s := newTestStore(t)
	agentID := id.NewAgentID()

	ctxA := cortex.WithScope(context.Background(), scopeOf("ws_a"))
	ctxB := cortex.WithScope(context.Background(), scopeOf("ws_b"))

	cpA := newTestCheckpoint(id.NewAgentRunID(), agentID)
	if err := s.CreateCheckpoint(ctxA, cpA); err != nil {
		t.Fatalf("create checkpoint A: %v", err)
	}
	cpB := newTestCheckpoint(id.NewAgentRunID(), agentID)
	if err := s.CreateCheckpoint(ctxB, cpB); err != nil {
		t.Fatalf("create checkpoint B: %v", err)
	}

	// GetCheckpoint from the wrong scope must refuse, not just filter.
	if _, err := s.GetCheckpoint(ctxB, cpA.ID); !errors.Is(err, cortex.ErrCheckpointNotFound) {
		t.Errorf("GetCheckpoint(ctxB, cpA.ID) = %v, want ErrCheckpointNotFound", err)
	}
	if _, err := s.GetCheckpoint(ctxA, cpA.ID); err != nil {
		t.Errorf("GetCheckpoint(ctxA, cpA.ID) = %v, want no error", err)
	}

	// ListPending/CountPending from ctxA must return only cpA.
	got, err := s.ListPending(ctxA, nil)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(got) != 1 || got[0].ID != cpA.ID {
		t.Fatalf("ListPending(ctxA) = %d checkpoint(s), want exactly cpA (predicate isn't filtering)", len(got))
	}

	count, err := s.CountPending(ctxA, nil)
	if err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountPending(ctxA) = %d, want 1", count)
	}

	// Resolve from the wrong scope must refuse and leave the checkpoint
	// untouched, rather than resolving a checkpoint outside the caller's
	// scope.
	if resolveErr := s.Resolve(ctxB, cpA.ID, checkpoint.Decision{Approved: true}); !errors.Is(resolveErr, cortex.ErrCheckpointNotFound) {
		t.Errorf("Resolve(ctxB, cpA.ID) = %v, want ErrCheckpointNotFound", resolveErr)
	}
	stillPending, err := s.GetCheckpoint(ctxA, cpA.ID)
	if err != nil {
		t.Fatalf("reload cpA: %v", err)
	}
	if stillPending.State != "pending" {
		t.Errorf("cpA.State after cross-scope Resolve = %q, want %q (must not resolve)", stillPending.State, "pending")
	}

	// Resolve from the correct scope must succeed.
	if err := s.Resolve(ctxA, cpA.ID, checkpoint.Decision{Approved: true}); err != nil {
		t.Fatalf("Resolve(ctxA, cpA.ID): %v", err)
	}
}
