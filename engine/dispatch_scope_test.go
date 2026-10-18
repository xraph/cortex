package engine

import (
	"context"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/llm"
)

func TestDirectDispatchRequiresValidScope(t *testing.T) {
	called := false
	e, err := New(WithTool(llm.Tool{Name: "effect"}, func(context.Context, cortex.Invocation) (string, error) { called = true; return "done", nil }))
	if err != nil {
		t.Fatal(err)
	}
	for _, ctx := range []context.Context{
		context.Background(),
		cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "a/b"}}}),
	} {
		e.Dispatch(ctx, "effect", `{}`)
		if called {
			t.Fatal("tool executed without valid scope")
		}
	}
}
