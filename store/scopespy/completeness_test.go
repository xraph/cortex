package scopespy

import (
	"context"
	"reflect"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
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

// reachedMethods runs two real scenarios against fresh Spy instances and
// unions the store.Store method names either one actually invoked:
//
//   - RunAgent, with a tool-calling LLM double so CreateToolCall is
//     exercised too, alongside every method a plain single-step run
//     already reaches.
//   - RunAgent again, this time against an external tool, so the
//     suspend path (CreateSuspension) is exercised. A run that finishes
//     normally never writes a suspension, so without this scenario the
//     Spy override for it would look dead.
//   - StreamAgent, which runs its store work (LoadConversation,
//     CreateStep, SaveConversation, UpdateRun, ...) from a goroutine
//     inside streamReAct — a separate code path from runReAct that could
//     reach a method the synchronous run above never does, or reach the
//     same methods through call sites RunAgent alone wouldn't exercise.
//     Running only RunAgent here would make the equality check in
//     TestSpy_OverriddenMethodsMatchWhatTheReactLoopReaches fail on any
//     override Spy carries solely for the streaming path.
func reachedMethods(t *testing.T) map[string]bool {
	t.Helper()
	reached := make(map[string]bool)

	const toolName = "spy-completeness-tool"
	runSpy := New()
	runEngine, err := engine.New(
		engine.WithStore(runSpy),
		engine.WithLLM(ToolCallingLLM(toolName)),
		engine.WithTool(
			llm.Tool{Name: toolName, Description: "test-only tool for completeness coverage"},
			func(_ context.Context, _ cortex.Invocation) (string, error) { return "ok", nil },
		),
	)
	if err != nil {
		t.Fatalf("engine.New (RunAgent): %v", err)
	}
	runCtx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
	if _, runErr := runEngine.RunAgent(runCtx, "assistant", "hello", nil); runErr != nil {
		t.Fatalf("RunAgent: %v", runErr)
	}
	for _, c := range runSpy.Calls() {
		reached[c.Method] = true
	}

	const externalToolName = "spy-completeness-external-tool"
	suspendSpy := New()
	suspendEngine, err := engine.New(
		engine.WithStore(suspendSpy),
		engine.WithLLM(ToolCallingLLM(externalToolName)),
		engine.WithExternalTool(llm.Tool{
			Name:        externalToolName,
			Description: "test-only external tool for completeness coverage",
		}),
	)
	if err != nil {
		t.Fatalf("engine.New (suspending RunAgent): %v", err)
	}
	suspended, err := suspendEngine.RunAgent(runCtx, "assistant", "hello", nil)
	if err != nil {
		t.Fatalf("RunAgent (suspending): %v", err)
	}
	if suspended.State != run.StatePaused {
		t.Fatalf("the suspend scenario left the run in %q, so it never reached CreateSuspension", suspended.State)
	}
	for _, c := range suspendSpy.Calls() {
		reached[c.Method] = true
	}

	streamSpy := New()
	streamEngine, err := engine.New(
		engine.WithStore(streamSpy),
		engine.WithLLM(StaticStreamLLM("done")),
	)
	if err != nil {
		t.Fatalf("engine.New (StreamAgent): %v", err)
	}
	streamCtx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
	events := make(chan engine.StreamEvent, 64)
	if err := streamEngine.StreamAgent(streamCtx, "assistant", "hello", nil, events); err != nil {
		t.Fatalf("StreamAgent: %v", err)
	}
	var drained int
	for range events {
		// Drain to completion: streamReAct closes events once its
		// goroutine finishes all store work, so this blocks until every
		// call below has already happened.
		drained++
	}
	if drained == 0 {
		t.Fatal("no stream events received; StreamAgent may not have run")
	}
	for _, c := range streamSpy.Calls() {
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
// proves what a real RunAgent run AND a real StreamAgent run actually
// touch, unioned (a tool-calling LLM double on the RunAgent side covers
// CreateToolCall, the one call site a plain non-tool run never reaches;
// StreamAgent covers the goroutine-based streamReAct path, which is a
// separate code path from the synchronous runReAct one). This test
// asserts the two sets are equal: if either loop starts calling
// something Spy doesn't override, or Spy gains an override nothing
// calls anymore, the mismatch says which side moved instead of a bare
// nil-pointer panic somewhere else.
func TestSpy_OverriddenMethodsMatchWhatTheReactLoopReaches(t *testing.T) {
	overridden := overriddenMethods(t)
	reached := reachedMethods(t)

	if len(reached) == 0 {
		t.Fatal("reachedMethods recorded no calls; this test proves nothing")
	}

	for name := range reached {
		if !overridden[name] {
			t.Errorf("RunAgent or StreamAgent called %q, but Spy doesn't override it (overriddenMethods disagrees with reachedMethods — investigate before trusting either)", name)
		}
	}
	for name := range overridden {
		if !reached[name] {
			t.Errorf("Spy overrides %q, but neither RunAgent nor StreamAgent called it in this test — either the override is dead, or reachedMethods needs a scenario that reaches it", name)
		}
	}
}
