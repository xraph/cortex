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
// from the context, but Update must never rewrite it, even though the
// agent store stays app-keyed this phase and Update takes no scope
// predicate at all. scopeOf is shared with run_scope_test.go.
func TestAgentStore_UpdateDoesNotMutateScope(t *testing.T) {
	s := newTestStore(t)
	createCtx := cortex.WithScope(context.Background(), scopeOf("ws_x", "proj_y"))

	cfg := &agent.Config{
		ID:          id.NewAgentID(),
		Name:        "immutable-scope",
		AppID:       "app1",
		Description: "original",
	}
	if err := s.Create(createCtx, cfg); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	const want = "workspace=ws_x/project=proj_y"

	loaded, err := s.Get(context.Background(), cfg.ID)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got := loaded.Scope.Canonical(); got != want {
		t.Fatalf("scope after create = %q, want %q", got, want)
	}
	loaded.Description = "mutated"

	// Agent Update doesn't use context scope for anything — no predicate,
	// no write. A plain background context proves the point: nothing
	// about the update path can put the row's scope back even if it
	// wanted to.
	if updateErr := s.Update(context.Background(), loaded); updateErr != nil {
		t.Fatalf("update agent: %v", updateErr)
	}

	reloaded, err := s.Get(context.Background(), cfg.ID)
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
