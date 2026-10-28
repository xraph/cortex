package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/knowledge"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/tool"
)

// catalogKnowledge answers nothing. Being non-nil is what makes the engine
// advertise the knowledge_search builtin.
type catalogKnowledge struct{}

func (catalogKnowledge) Retrieve(context.Context, string, *knowledge.RetrieveParams) ([]knowledge.ScoredChunk, error) {
	return nil, nil
}

func (catalogKnowledge) ListCollections(context.Context) ([]knowledge.CollectionInfo, error) {
	return nil, nil
}

func catalogHandler(context.Context, cortex.Invocation) (string, error) { return "ok", nil }

func TestToolsListsKindsInDispatchOrder(t *testing.T) {
	e, err := engine.New(
		engine.WithTool(llm.Tool{Name: "a"}, catalogHandler),
		engine.WithExternalTool(llm.Tool{Name: "b"}),
		engine.WithKnowledge(catalogKnowledge{}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	want := []struct {
		name string
		kind engine.ToolKind
	}{
		{"knowledge_search", engine.ToolBuiltin},
		{"a", engine.ToolRegistered},
		{"b", engine.ToolExternal},
	}
	got := e.Tools()
	if len(got) != len(want) {
		t.Fatalf("Tools() has %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Def.Name != w.name || got[i].Kind != w.kind {
			t.Errorf("Tools()[%d] = %s/%s, want %s/%s", i, got[i].Def.Name, got[i].Kind, w.name, w.kind)
		}
	}
}

func TestWithToolRetrySafeMarksTools(t *testing.T) {
	e, err := engine.New(
		engine.WithTool(llm.Tool{Name: "a"}, catalogHandler),
		engine.WithTool(llm.Tool{Name: "b"}, catalogHandler),
		engine.WithToolRetrySafe("a"),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, info := range e.Tools() {
		if want := info.Def.Name == "a"; info.RetrySafe != want {
			t.Errorf("%s RetrySafe = %v, want %v", info.Def.Name, info.RetrySafe, want)
		}
	}
	if !e.ToolRetrySafe("a") || e.ToolRetrySafe("b") {
		t.Errorf("ToolRetrySafe a=%v b=%v, want true false", e.ToolRetrySafe("a"), e.ToolRetrySafe("b"))
	}
}

func TestWithToolRetrySafeRejectsUnknownName(t *testing.T) {
	_, err := engine.New(engine.WithToolRetrySafe("ghost"))
	if err == nil || !strings.Contains(err.Error(), `retry-safe tool "ghost" is not registered`) {
		t.Fatalf("New err = %v, want the unregistered tool error", err)
	}
}

type registerInput struct {
	Q string `json:"q"`
}

func TestRegisterAddsRetrySafe(t *testing.T) {
	e, err := engine.New(
		tool.Register("lookup", "look something up", func(context.Context, cortex.Invocation, registerInput) (string, error) {
			return "ok", nil
		}, tool.RetrySafe()),
		tool.Register("write", "write something", func(context.Context, cortex.Invocation, registerInput) (string, error) {
			return "ok", nil
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !e.ToolRetrySafe("lookup") {
		t.Error("lookup should be retry-safe")
	}
	if e.ToolRetrySafe("write") {
		t.Error("write should not be retry-safe")
	}
}

func TestToolsListsOnlyTheDefinitionDispatchReaches(t *testing.T) {
	e, err := engine.New(
		engine.WithTool(llm.Tool{Name: "a", Description: "first"}, catalogHandler),
		engine.WithTool(llm.Tool{Name: "a", Description: "shadowed"}, catalogHandler),
		engine.WithExternalTool(llm.Tool{Name: "a", Description: "external"}),
		engine.WithExternalTool(llm.Tool{Name: "b"}),
		engine.WithKnowledge(catalogKnowledge{}),
		engine.WithTool(llm.Tool{Name: "knowledge_search", Description: "shadowed by builtin"}, catalogHandler),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := e.Tools()
	if len(got) != 3 {
		t.Fatalf("Tools() = %+v, want 3 entries", got)
	}
	if got[0].Def.Name != "knowledge_search" || got[0].Kind != engine.ToolBuiltin {
		t.Errorf("entry 0 = %+v", got[0])
	}
	if got[1].Def.Description != "first" || got[1].Kind != engine.ToolRegistered {
		t.Errorf("entry 1 = %+v", got[1])
	}
	if got[2].Def.Name != "b" {
		t.Errorf("entry 2 = %+v", got[2])
	}
}
