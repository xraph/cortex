package tool_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/tool"
)

type listInput struct {
	Project string `json:"project" jsonschema:"the project id"`
	Limit   int    `json:"limit,omitempty"`
}

type listOutput struct {
	Names []string `json:"names"`
}

func listFn(context.Context, cortex.Invocation, listInput) (listOutput, error) {
	return listOutput{Names: []string{"a", "b"}}, nil
}

func invoke(args string) cortex.Invocation {
	return cortex.Invocation{Call: llm.ToolCall{ID: "c1", Name: "list", Arguments: args}}
}

func TestNewGeneratesObjectSchema(t *testing.T) {
	def, _ := tool.New("list", "list things", listFn)
	p, ok := def.Parameters.(map[string]any)
	if !ok {
		t.Fatalf("Parameters = %T, want map[string]any", def.Parameters)
	}
	if p["type"] != "object" {
		t.Fatalf("type = %v, want object", p["type"])
	}
	props, ok := p["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %T", p["properties"])
	}
	project, ok := props["project"].(map[string]any)
	if !ok || project["description"] != "the project id" {
		t.Errorf("project property = %v", props["project"])
	}
	if !reflect.DeepEqual(p["required"], []any{"project"}) {
		t.Errorf("required = %#v, want [project]", p["required"])
	}
	if p["additionalProperties"] != false {
		t.Errorf("additionalProperties = %#v, want false", p["additionalProperties"])
	}
	for _, k := range []string{"$schema", "$id"} {
		if _, ok := p[k]; ok {
			t.Errorf("schema carries %s", k)
		}
	}
	// Numbers must be json.Number, not float64, to match the builtin shape.
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case float64:
			t.Errorf("found float64 %v in schema", x)
		case map[string]any:
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(p)
}

func TestSchemaGolden(t *testing.T) {
	s, err := tool.Schema[listInput]()
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	got, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "list_input.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("schema drifted\n got: %s\nwant: %s", got, want)
	}
}

func TestNewPanicsWhenInputIsNotAStruct(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("New did not panic")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, `tool "x": input must be a struct`) {
			t.Errorf("panic = %q", msg)
		}
	}()
	tool.New("x", "bad", func(context.Context, cortex.Invocation, string) (string, error) { return "", nil })
}

func TestHandlerValidatesArguments(t *testing.T) {
	calls := 0
	_, h := tool.New("list", "list", func(ctx context.Context, inv cortex.Invocation, in listInput) (listOutput, error) {
		calls++
		return listFn(ctx, inv, in)
	})

	tests := []struct {
		name, args, wantErr string
		wantCalls           int
	}{
		{"wrong type", `{"project":"p","limit":"x"}`, "limit", 0},
		{"missing required", `{}`, "project", 0},
		{"empty string is empty object", ``, "project", 0},
		{"extra property", `{"project":"p","extra":1}`, "extra", 0},
		{"valid", `{"project":"p","limit":3}`, "", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls = 0
			_, err := h(context.Background(), invoke(tc.args))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
			} else if !strings.Contains(err.Error(), `tool "list": invalid arguments`) {
				t.Errorf("err = %v, want the invalid arguments prefix", err)
			}
			if calls != tc.wantCalls {
				t.Errorf("fn called %d times, want %d", calls, tc.wantCalls)
			}
		})
	}
}

func TestHandlerEncodesOutput(t *testing.T) {
	_, structH := tool.New("list", "list", listFn)
	got, err := structH(context.Background(), invoke(`{"project":"p"}`))
	if err != nil || got != `{"names":["a","b"]}` {
		t.Errorf("struct out = %q, %v", got, err)
	}

	_, strH := tool.New("s", "s", func(context.Context, cortex.Invocation, listInput) (string, error) { return "plain text", nil })
	if got, err := strH(context.Background(), invoke(`{"project":"p"}`)); err != nil || got != "plain text" {
		t.Errorf("string out = %q, %v", got, err)
	}

	_, rawH := tool.New("r", "r", func(context.Context, cortex.Invocation, listInput) (json.RawMessage, error) {
		return json.RawMessage(`{"already":  "json"}`), nil
	})
	if got, err := rawH(context.Background(), invoke(`{"project":"p"}`)); err != nil || got != `{"already":  "json"}` {
		t.Errorf("raw out = %q, %v", got, err)
	}
}

func TestHandlerPassesEffectUncertainThrough(t *testing.T) {
	_, h := tool.New("list", "list", func(context.Context, cortex.Invocation, listInput) (string, error) {
		return "", fmt.Errorf("x: %w", cortex.ErrEffectUncertain)
	})
	_, err := h(context.Background(), invoke(`{"project":"p"}`))
	if !errors.Is(err, cortex.ErrEffectUncertain) {
		t.Errorf("err = %v, want ErrEffectUncertain", err)
	}
}

func TestSchemaIsStableAcrossCalls(t *testing.T) {
	a, _ := tool.New("list", "list", listFn)
	b, _ := tool.New("list", "list", listFn)
	if !reflect.DeepEqual(a.Parameters, b.Parameters) {
		t.Error("Parameters differ between calls")
	}
	ja, _ := json.Marshal(a.Parameters)
	jb, _ := json.Marshal(b.Parameters)
	if string(ja) != string(jb) {
		t.Errorf("bytes differ:\n%s\n%s", ja, jb)
	}
}

// onceLLM asks for one tool call and then finishes.
type onceLLM struct {
	mu   sync.Mutex
	done bool
	call llm.ToolCall
}

func (m *onceLLM) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.done {
		m.done = true
		return &llm.Response{ToolCalls: []llm.ToolCall{m.call}}, nil
	}
	return &llm.Response{Content: "done", FinishReason: "stop"}, nil
}

func (m *onceLLM) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, errors.New("onceLLM: streaming not supported")
}

func TestTypedToolRunsInEngine(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_tool"}}})

	drv := sqlitedriver.New()
	if err := drv.Open(ctx, filepath.Join(t.TempDir(), "tool.db")); err != nil {
		t.Fatalf("open sqlite driver: %v", err)
	}
	db, err := grove.Open(drv)
	if err != nil {
		t.Fatalf("grove open: %v", err)
	}
	store := sqlite.New(db)
	if err = store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	model := &onceLLM{call: llm.ToolCall{ID: "call-1", Name: "list", Arguments: `{"project":"p"}`}}
	eng, err := engine.New(
		engine.WithStore(store),
		engine.WithLLM(model),
		tool.Register("list", "list things", listFn, tool.RetrySafe()),
	)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err = eng.CreateAgent(ctx, &agent.Config{ID: id.NewAgentID(), Name: "assistant", MaxSteps: 3}); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	r, err := eng.RunAgent(ctx, "assistant", "list", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if r.State != run.StateCompleted {
		t.Fatalf("run state = %q, want completed", r.State)
	}

	steps, err := store.ListSteps(ctx, r.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	var results []string
	for _, s := range steps {
		calls, err := store.ListToolCalls(ctx, s.ID)
		if err != nil {
			t.Fatalf("ListToolCalls: %v", err)
		}
		for _, c := range calls {
			results = append(results, c.Result)
		}
	}
	if len(results) != 1 || results[0] != `{"names":["a","b"]}` {
		t.Errorf("tool call results = %q, want the JSON of Out", results)
	}
}
