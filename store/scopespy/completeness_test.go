package scopespy

import (
	"context"
	"reflect"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/store"
)

// overriddenMethods probes every method on the store.Store interface by
// calling it via reflection on a fresh Spy, with zero-value arguments and
// a scoped ctx wherever a ctx parameter is expected. Spy embeds
// store.Store as a nil interface and overrides only what it needs to, so
// dispatching a call to any method it hasn't explicitly overridden
// panics on the nil interface before the callee's body ever runs — that
// happens regardless of the arguments, since a nil interface has no
// concrete type to dispatch to. A method that returns cleanly and gets
// recorded therefore proves Spy overrides it; this is the same
// call-and-recover technique the old hand-maintained version of this
// test used, just run across the full interface instead of a fixed list.
func overriddenMethods(t *testing.T) map[string]bool {
	t.Helper()
	storeType := reflect.TypeOf((*store.Store)(nil)).Elem()
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})

	overridden := make(map[string]bool, storeType.NumMethod())
	for i := 0; i < storeType.NumMethod(); i++ {
		name := storeType.Method(i).Name
		spy := New()
		mv := reflect.ValueOf(spy).MethodByName(name)
		if !mv.IsValid() {
			t.Fatalf("*Spy has no method %q despite store.Store requiring it", name)
		}

		mt := mv.Type()
		args := make([]reflect.Value, mt.NumIn())
		for j := range args {
			pt := mt.In(j)
			if pt == ctxType {
				args[j] = reflect.ValueOf(ctx)
				continue
			}
			args[j] = reflect.Zero(pt)
		}

		func() {
			defer func() { _ = recover() }() // not overridden: fell through to the embedded nil Store
			mv.Call(args)
		}()

		if spy.CallCount() == 1 {
			overridden[name] = true
		}
	}
	return overridden
}

// reachedMethods runs a real RunAgent call — with a tool-calling LLM
// double so CreateToolCall is exercised too, alongside every method a
// plain single-step run already reaches — against a fresh Spy, and
// returns the set of store.Store method names it actually invoked.
func reachedMethods(t *testing.T) map[string]bool {
	t.Helper()
	spy := New()
	const toolName = "spy-completeness-tool"
	e, err := engine.New(
		engine.WithStore(spy),
		engine.WithLLM(ToolCallingLLM(toolName)),
		engine.WithTool(
			llm.Tool{Name: toolName, Description: "test-only tool for completeness coverage"},
			func(_ context.Context, _ string) (string, error) { return "ok", nil },
		),
	)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
	if _, err := e.RunAgent(ctx, "app1", "assistant", "hello", nil); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	reached := make(map[string]bool)
	for _, c := range spy.Calls() {
		reached[c.Method] = true
	}
	return reached
}

// TestSpy_OverriddenMethodsMatchWhatTheReactLoopReaches replaces a
// hand-maintained inventory of "the methods the react loop reaches" with
// two independently derived sets compared against each other, closing a
// gap the old fixed-list version had: a fixed list only fails on a
// rename or a dropped Spy override, never on a genuinely new call site
// the loop starts making, because such a call would simply panic
// wherever the loop next runs against Spy — a real failure, but not one
// this test attributed to a completeness gap.
//
// overriddenMethods proves what Spy actually overrides; reachedMethods
// proves what a real RunAgent run actually touches (using a
// tool-calling LLM double so CreateToolCall, the one call site a plain
// non-tool run never reaches, is covered too). This test asserts the two
// sets are equal: if the loop starts calling something Spy doesn't
// override, or Spy gains an override nothing calls anymore, the mismatch
// says which side moved instead of a bare nil-pointer panic somewhere
// else.
func TestSpy_OverriddenMethodsMatchWhatTheReactLoopReaches(t *testing.T) {
	overridden := overriddenMethods(t)
	reached := reachedMethods(t)

	if len(reached) == 0 {
		t.Fatal("reachedMethods recorded no calls; this test proves nothing")
	}

	for name := range reached {
		if !overridden[name] {
			t.Errorf("RunAgent called %q, but Spy doesn't override it (overriddenMethods disagrees with reachedMethods — investigate before trusting either)", name)
		}
	}
	for name := range overridden {
		if !reached[name] {
			t.Errorf("Spy overrides %q, but this test's RunAgent run never called it — either the override is dead, or reachedMethods needs a scenario that reaches it", name)
		}
	}
}
