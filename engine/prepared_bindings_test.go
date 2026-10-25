package engine_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
)

type bindingModel struct{ t *testing.T }

func (m bindingModel) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	m.t.Fatal("binding check called model")
	return nil, nil
}
func (m bindingModel) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	m.t.Fatal("binding check streamed model")
	return nil, nil
}

func TestPreparedBindings(t *testing.T) {
	for _, change := range []string{"unchanged", "derived", "subset", "empty", "model_unpinned", "tool_unpinned", "model_changed", "tool_changed", "model_removed", "tool_removed", "nil_handler", "external_only", "schema_changed", "schema_invalid", "schema_equivalent", "duplicate", "scope", "no_scope", "cancelled", "nil_prepared", "no_model", "typed_nil_model", "tool_pin_removed", "reserved_name"} {
		t.Run(change, func(t *testing.T) {
			ctx := overlayCtx("bindings")
			s := newSQLiteStoreForTest(t)
			ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", Model: "model"}
			if err := s.Create(ctx, ag); err != nil {
				t.Fatal(err)
			}
			tool := llm.Tool{Name: "probe", Description: "original", Parameters: map[string]any{"const": int64(9007199254740993)}}
			handler := func(context.Context, cortex.Invocation) (string, error) {
				t.Fatal("binding check executed tool")
				return "", nil
			}
			opts := []engine.Option{engine.WithStore(s), engine.WithLLM(bindingModel{t}), engine.WithTool(tool, handler)}
			if change != "model_unpinned" {
				opts = append(opts, engine.WithModelRevision("model", "m1"))
			}
			if change != "tool_unpinned" {
				opts = append(opts, engine.WithToolRevision("probe", "t1"))
			}
			preparer, err := engine.New(opts...)
			if err != nil {
				t.Fatal(err)
			}
			p, err := preparer.PrepareAgent(ctx, ag.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			if change == "reserved_name" {
				tool.Name = "knowledge_search"
				preparer, err = engine.New(engine.WithStore(s), engine.WithLLM(bindingModel{t}), engine.WithModelRevision("model", "m1"), engine.WithToolRevision(tool.Name, "t1"), engine.WithTool(tool, handler))
				if err != nil {
					t.Fatal(err)
				}
				p, err = preparer.PrepareAgent(ctx, ag.ID, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			current := []engine.Option{engine.WithLLM(bindingModel{t}), engine.WithModelRevision("model", "m1"), engine.WithToolRevision("probe", "t1")}
			want := cortex.ErrInvalidPrepared
			switch change {
			case "unchanged":
				want = nil
			case "derived":
				p, err = p.Derive(engine.PreparedEdits{ToolDescriptions: map[string]string{"probe": "candidate guidance"}, Temperature: new(.25)})
				want = nil
			case "subset":
				current = append(current, engine.WithTool(llm.Tool{Name: "unselected"}, nil))
				want = nil
			case "empty":
				p, err = p.Derive(engine.PreparedEdits{Tools: new([]string{})})
				current = current[:0]
				current = append(current, engine.WithLLM(bindingModel{t}), engine.WithModelRevision("model", "m1"))
				want = nil
			case "model_changed":
				current = append(current, engine.WithModelRevision("model", "m2"))
			case "tool_changed":
				current = append(current, engine.WithToolRevision("probe", "t2"))
			case "model_removed":
				current = append(current[:0], engine.WithLLM(bindingModel{t}), engine.WithToolRevision("probe", "t1"))
			case "nil_handler":
				handler = nil
			case "external_only":
				current = append(current, engine.WithExternalTool(tool))
			case "schema_changed":
				tool.Parameters = map[string]any{"const": int64(9007199254740992)}
			case "schema_invalid":
				tool.Parameters = make(chan int)
			case "schema_equivalent":
				tool.Parameters = json.RawMessage(`{ "const": 9007199254740993 }`)
				want = nil
			case "duplicate":
				current = append(current, engine.WithTool(tool, handler))
			case "scope":
				ctx = overlayCtx("other")
				want = cortex.ErrInvalidScope
			case "no_scope":
				ctx = context.Background()
				want = cortex.ErrNoScope
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "nil_prepared":
				p = nil
			case "typed_nil_model":
				current = append(current, engine.WithLLM((*bindingModel)(nil)))
				want = cortex.ErrNoLLM
			case "tool_pin_removed":
				current = append(current[:0], engine.WithLLM(bindingModel{t}), engine.WithModelRevision("model", "m1"))
			case "reserved_name":
				current = append(current, engine.WithToolRevision(tool.Name, "t1"))
			case "no_model":
				current = append(current, engine.WithLLM(nil))
				want = cortex.ErrNoLLM
			}
			if err != nil {
				t.Fatal(err)
			}
			if change != "tool_removed" && change != "external_only" && change != "empty" {
				current = append(current, engine.WithTool(tool, handler))
			}
			checker, err := engine.New(current...)
			if err != nil {
				t.Fatal(err)
			}
			err = checker.CheckPreparedBindings(ctx, p)
			if !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
		})
	}
}

func TestPreparedBindingsBuiltin(t *testing.T) {
	ctx := overlayCtx("builtin-binding")
	s := newSQLiteStoreForTest(t)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", Model: "model"}
	if err := s.Create(ctx, ag); err != nil {
		t.Fatal(err)
	}
	opts := []engine.Option{engine.WithLLM(bindingModel{t}), engine.WithModelRevision("model", "m1"), engine.WithToolRevision("knowledge_search", "k1")}
	preparer, err := engine.New(append(opts, engine.WithStore(s), engine.WithKnowledge(staticKnowledge{}))...)
	if err != nil {
		t.Fatal(err)
	}
	p, err := preparer.PrepareAgent(ctx, ag.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"present", "missing", "typed_nil"} {
		t.Run(state, func(t *testing.T) {
			current := append([]engine.Option(nil), opts...)
			switch state {
			case "present":
				current = append(current, engine.WithKnowledge(staticKnowledge{}))
			case "typed_nil":
				current = append(current, engine.WithKnowledge((*staticKnowledge)(nil)))
			}
			checker, buildErr := engine.New(current...)
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			checkErr := checker.CheckPreparedBindings(ctx, p)
			if state == "present" {
				if checkErr != nil {
					t.Fatal(checkErr)
				}
			} else if !errors.Is(checkErr, cortex.ErrInvalidPrepared) {
				t.Fatalf("absent builtin accepted: %v", checkErr)
			}
		})
	}
}

func TestPreparedBindingsRejectMalformedManifest(t *testing.T) {
	ctx := overlayCtx("manifest-binding")
	s := newSQLiteStoreForTest(t)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", Model: "model"}
	if err := s.Create(ctx, ag); err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(engine.WithStore(s), engine.WithLLM(bindingModel{t}), engine.WithModelRevision("model", "m1"), engine.WithToolRevision("probe", "t1"), engine.WithTool(llm.Tool{Name: "probe"}, noopHandler))
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.PrepareAgent(ctx, ag.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"missing", "extra", "name", "digest", "blank_revision", "duplicate_tool"} {
		t.Run(change, func(t *testing.T) {
			data, marshalErr := json.Marshal(p)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			var wire struct {
				Digest   string         `json:"digest"`
				Snapshot map[string]any `json:"snapshot"`
			}
			if decodeErr := json.Unmarshal(data, &wire); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			provenance := wire.Snapshot["provenance"].(map[string]any)
			pins := provenance["tools"].([]any)
			pin := pins[0].(map[string]any)
			switch change {
			case "missing":
				provenance["tools"] = []any{}
			case "extra":
				provenance["tools"] = append(pins, pin)
			case "name":
				pin["name"] = "other"
			case "digest":
				pin["digest"] = "changed"
			case "blank_revision":
				pin["revision"] = " "
			case "duplicate_tool":
				wire.Snapshot["tools"] = append(wire.Snapshot["tools"].([]any), wire.Snapshot["tools"].([]any)[0])
				provenance["tools"] = append(pins, pin)
			}
			payload, marshalErr := json.Marshal(wire.Snapshot)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			digest := sha256.Sum256(payload)
			wire.Digest = hex.EncodeToString(digest[:])
			data, marshalErr = json.Marshal(wire)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			var malformed engine.PreparedAgent
			if decodeErr := json.Unmarshal(data, &malformed); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if checkErr := e.CheckPreparedBindings(ctx, &malformed); !errors.Is(checkErr, cortex.ErrInvalidPrepared) {
				t.Fatalf("malformed manifest accepted: %v", checkErr)
			}
		})
	}
}
