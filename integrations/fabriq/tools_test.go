package fabriqbrain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xraph/fabriq/core/agent"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
)

type fakeToolLister struct{ tools []agent.Tool }

func (f fakeToolLister) Tools() []agent.Tool { return f.tools }

func newEngineWith(t *testing.T, opts ...engine.Option) *engine.Engine {
	t.Helper()
	e, err := engine.New(opts...)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return e
}

func toolContext() context.Context {
	return cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "fabriq-test"}}})
}

func TestToolOptions_SkipsRecallAndAdaptsHandlers(t *testing.T) {
	called := ""
	tl := fakeToolLister{tools: []agent.Tool{
		{Name: "recall", Description: "x", InputSchema: json.RawMessage(`{"type":"object"}`),
			Handler: func(context.Context, json.RawMessage) (any, error) { return nil, nil }},
		{Name: "graph_traverse", Description: "walk edges", InputSchema: json.RawMessage(`{"type":"object"}`),
			Handler: func(_ context.Context, args json.RawMessage) (any, error) {
				called = string(args)
				return map[string]any{"ok": true}, nil
			}},
	}}

	opts := toolOptions(tl, defaultConfig())
	if len(opts) != 1 {
		t.Fatalf("got %d tool options, want 1 (recall skipped)", len(opts))
	}

	// Apply to a real engine and dispatch the registered tool.
	eng := newEngineWith(t, opts...)
	if denied := eng.Dispatch(context.Background(), "graph_traverse", `{"from":"n1"}`); !strings.Contains(denied, cortex.ErrNoScope.Error()) || called != "" {
		t.Fatalf("unscoped dispatch reached handler: result=%q args=%q", denied, called)
	}
	got := eng.Dispatch(toolContext(), "graph_traverse", `{"from":"n1"}`)
	if called != `{"from":"n1"}` {
		t.Fatalf("handler got args %q", called)
	}
	if got != `{"ok":true}` {
		t.Fatalf("tool result = %q, want {\"ok\":true}", got)
	}
}
