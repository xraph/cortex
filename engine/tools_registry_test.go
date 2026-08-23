package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/llm"
)

func echoTool() (llm.Tool, ToolHandler) {
	def := llm.Tool{
		Name:        "echo",
		Description: "echoes its arguments",
		Parameters:  map[string]any{"type": "object"},
	}
	h := func(_ context.Context, inv cortex.Invocation) (string, error) {
		return "echoed:" + inv.Call.Arguments, nil
	}
	return def, h
}

func TestWithTool_AdvertisedInResolveTools(t *testing.T) {
	def, h := echoTool()
	e, err := New(WithTool(def, h))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tools := e.resolveTools(context.Background(), cortex.Subject{}, nil)
	var found bool
	for _, tl := range tools {
		if tl.Name == "echo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("resolveTools did not include registered tool %q; got %d tools", "echo", len(tools))
	}
}

func TestWithTool_DispatchedInExecuteTool(t *testing.T) {
	def, h := echoTool()
	e, err := New(WithTool(def, h))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, _ := e.executeTool(context.Background(), cortex.Subject{}, llm.ToolCall{Name: "echo", Arguments: `{"x":1}`})
	if got != `echoed:{"x":1}` {
		t.Fatalf("executeTool = %q, want %q", got, `echoed:{"x":1}`)
	}
}

func TestExecuteTool_UnknownStillErrors(t *testing.T) {
	e, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, _ := e.executeTool(context.Background(), cortex.Subject{}, llm.ToolCall{Name: "nope"})
	if !strings.Contains(got, "unknown tool") {
		t.Fatalf("executeTool = %q, want it to contain %q", got, "unknown tool")
	}
}
