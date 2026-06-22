package fabriqbrain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"
	"github.com/xraph/vessel"

	"github.com/xraph/cortex/engine"
)

func TestEngineOptions_NoFabriqFacadeIsNoop(t *testing.T) {
	c := vessel.New() // empty container: no *fabriq.Fabriq provided
	opts := EngineOptions(c)
	if opts != nil {
		t.Fatalf("EngineOptions with no facade = %v, want nil", opts)
	}
	// EngineOption must be a safe no-op that applies cleanly.
	e, err := engine.New(EngineOption(c))
	if err != nil {
		t.Fatalf("engine.New with no-op EngineOption: %v", err)
	}
	if e.Knowledge() != nil {
		t.Fatalf("knowledge should be nil when no facade present")
	}
}

type fakeBrain struct {
	pack  agent.ContextPack
	tools []agent.Tool
	reqs  []agent.RememberRequest
}

func (f *fakeBrain) Recall(context.Context, agent.RecallRequest) (agent.ContextPack, error) {
	return f.pack, nil
}
func (f *fakeBrain) Tools() []agent.Tool { return f.tools }
func (f *fakeBrain) Remember(_ context.Context, r agent.RememberRequest) (command.Result, error) {
	f.reqs = append(f.reqs, r)
	return command.Result{}, nil
}

func TestEngineOptions_PopulatedBundleWiresEverything(t *testing.T) {
	fb := &fakeBrain{tools: []agent.Tool{
		{Name: "recall", InputSchema: json.RawMessage(`{"type":"object"}`),
			Handler: func(context.Context, json.RawMessage) (any, error) { return nil, nil }},
		{Name: "graph_traverse", InputSchema: json.RawMessage(`{"type":"object"}`),
			Handler: func(context.Context, json.RawMessage) (any, error) { return map[string]any{"ok": true}, nil }},
	}}

	opts := engineOptions(fb, defaultConfig(), nil)
	e, err := engine.New(opts...)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if e.Knowledge() == nil {
		t.Fatalf("knowledge provider not wired")
	}
	if got := e.Dispatch(context.Background(), "graph_traverse", "{}"); got != `{"ok":true}` {
		t.Fatalf("graph_traverse dispatch = %q", got)
	}
	if got := e.Dispatch(context.Background(), "recall", "{}"); !strings.Contains(got, "unknown tool") {
		t.Fatalf("recall should be skipped, got %q", got)
	}
	found := false
	for _, ext := range e.Extensions().Extensions() {
		if ext.Name() == "fabriq-brain" {
			found = true
		}
	}
	if !found {
		t.Fatalf("learning-loop plugin not registered")
	}
}
