package sqlite

import (
	"context"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
)

// TestAgentStore_UpdateDoesNotMutateScope mirrors
// TestRunStore_UpdateDoesNotMutateScope for the agent store. An agent's
// scope is immutable after creation just like a run's: Create stamps it
// from the context, and Update — now scope-guarded like every other
// method — accepts a broader-but-still-matching context as authorization
// without ever collapsing the row's own narrower stored scope down to it.
// scopeOf is shared with run_scope_test.go.
func TestAgentStore_UpdateDoesNotMutateScope(t *testing.T) {
	s := newTestStore(t)
	createCtx := cortex.WithScope(context.Background(), scopeOf("ws_x", "proj_y"))

	cfg := &agent.Config{
		ID:          id.NewAgentID(),
		Name:        "immutable-scope",
		Description: "original",
	}
	if err := s.Create(createCtx, cfg); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	const want = "workspace=ws_x/project=proj_y"

	loaded, err := s.Get(createCtx, cfg.ID)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got := loaded.Scope.Canonical(); got != want {
		t.Fatalf("scope after create = %q, want %q", got, want)
	}
	loaded.Description = "mutated"

	// Reject the broad context, then perform the exact-scope update.
	updateCtx := cortex.WithScope(context.Background(), scopeOf("ws_x"))
	if deniedErr := s.Update(updateCtx, loaded); deniedErr == nil {
		t.Fatal("ancestor context authorized a mutation")
	}
	if updateErr := s.Update(createCtx, loaded); updateErr != nil {
		t.Fatalf("update agent: %v", updateErr)
	}

	reloaded, err := s.Get(createCtx, cfg.ID)
	if err != nil {
		t.Fatalf("reload agent: %v", err)
	}
	if reloaded.Scope.Canonical() != want {
		t.Errorf("scope after update = %q, want %q (scope must be immutable)", reloaded.Scope.Canonical(), want)
	}
	if reloaded.Description != "mutated" {
		t.Errorf("Description after update = %q, want %q", reloaded.Description, "mutated")
	}
}
