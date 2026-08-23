package cortex_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/llm"
)

type denyAll struct{}

func (denyAll) Visible(_ context.Context, _ cortex.Subject, _ []llm.Tool) []llm.Tool { return nil }
func (denyAll) Authorize(_ context.Context, _ cortex.Subject, _ llm.ToolCall) error {
	return errors.New("denied")
}

// The interface must be satisfiable by a host type that knows nothing
// about cortex's internals: a Subject and a call in, a decision out.
func TestToolAuthorizer_SatisfiedByAHostType(t *testing.T) {
	var a cortex.ToolAuthorizer = denyAll{}

	if got := a.Visible(context.Background(), cortex.Subject{}, []llm.Tool{{Name: "x"}}); len(got) != 0 {
		t.Errorf("Visible returned %d tools from a deny-all authorizer, want 0", len(got))
	}
	if err := a.Authorize(context.Background(), cortex.Subject{}, llm.ToolCall{Name: "x"}); err == nil {
		t.Error("Authorize returned nil from a deny-all authorizer")
	}
}

// Invocation must carry the Subject's fields, not merely reference them,
// so a tool handler receives scope and principal explicitly rather than
// having to pull them off the context.
func TestInvocation_CarriesSubjectAndCall(t *testing.T) {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}}
	inv := cortex.Invocation{
		Subject: cortex.Subject{Scope: scope, Principal: "svc-1"},
		Call:    llm.ToolCall{ID: "call_1", Name: "search"},
	}

	if inv.Scope.Canonical() != "workspace=ws_x" {
		t.Errorf("scope = %q, want workspace=ws_x", inv.Scope.Canonical())
	}
	if inv.Principal != "svc-1" {
		t.Errorf("principal = %v, want svc-1", inv.Principal)
	}
	if inv.Call.Name != "search" {
		t.Errorf("call name = %q, want search", inv.Call.Name)
	}
}
