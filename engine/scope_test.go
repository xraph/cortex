package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/store/scopespy"
)

// Both tests call RunAgent with an explicit appID: the app vocabulary
// (WithApp, AppFromContext, Config.AppID, and this parameter) is staying
// for now. Task 9 only removes cortex's own tenant vocabulary in favour
// of the host-defined Scope.

func TestRun_RejectsZeroScope(t *testing.T) {
	spy := scopespy.New()
	e, err := engine.New(engine.WithStore(spy))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	_, err = e.RunAgent(context.Background(), "app1", "assistant", "hello", nil)
	if !errors.Is(err, cortex.ErrNoScope) {
		t.Fatalf("err = %v, want ErrNoScope", err)
	}
	if spy.CallCount() != 0 {
		t.Errorf("store was called %d times on a zero scope; want 0", spy.CallCount())
	}
}

// TestRun_EveryStoreCallCarriesScope is the regression guard for the
// cross-tenant conversation bleed. The react loop used to pass "" as the
// tenant on all four conversation calls, so every tenant shared one
// history bucket. The spy fails the test if any recorded call arrives
// with a zero scope, which is what a future forgotten call site looks like.
func TestRun_EveryStoreCallCarriesScope(t *testing.T) {
	spy := scopespy.New()
	e, err := engine.New(engine.WithStore(spy), engine.WithLLM(scopespy.StaticLLM("done")))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	want := cortex.Scope{Levels: []cortex.Level{
		{Key: "workspace", Value: "ws_x"},
		{Key: "project", Value: "proj_y"},
	}}
	ctx := cortex.WithScope(context.Background(), want)

	if _, err := e.RunAgent(ctx, "app1", "assistant", "hello", nil); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	if spy.CallCount() == 0 {
		t.Fatal("spy recorded no store calls; the test proves nothing")
	}
	for _, c := range spy.Calls() {
		if c.Scope.IsZero() {
			t.Errorf("%s received a zero scope", c.Method)
		}
		if c.Scope.Canonical() != want.Canonical() {
			t.Errorf("%s got scope %q, want %q", c.Method, c.Scope.Canonical(), want.Canonical())
		}
	}
}

// TestStreamAgent_EveryStoreCallCarriesScope is the streaming sibling of
// TestRun_EveryStoreCallCarriesScope. streamReAct does its store work
// (LoadConversation, CreateStep, SaveConversation, UpdateRun) on a
// goroutine that TestRun_EveryStoreCallCarriesScope never exercises — a
// scope dropped only on that goroutine would pass the synchronous test
// and reach production unnoticed. This test drains the events channel to
// completion before touching the spy: reading spy.Calls() while the
// goroutine is still writing would race it and could pass on a stale,
// incomplete call list.
func TestStreamAgent_EveryStoreCallCarriesScope(t *testing.T) {
	spy := scopespy.New()
	e, err := engine.New(engine.WithStore(spy), engine.WithLLM(scopespy.StaticStreamLLM("done")))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	want := cortex.Scope{Levels: []cortex.Level{
		{Key: "workspace", Value: "ws_x"},
		{Key: "project", Value: "proj_y"},
	}}
	ctx := cortex.WithScope(context.Background(), want)

	events := make(chan engine.StreamEvent, 64)
	if err := e.StreamAgent(ctx, "app1", "assistant", "hello", nil, events); err != nil {
		t.Fatalf("StreamAgent: %v", err)
	}

	// Drain to completion. streamReAct closes events once its goroutine
	// finishes all store work, so this blocks until every call below has
	// already happened.
	var drained int
	for range events {
		drained++
	}
	if drained == 0 {
		t.Fatal("no stream events received; StreamAgent may not have run")
	}

	if spy.CallCount() == 0 {
		t.Fatal("spy recorded no store calls; the test proves nothing")
	}
	for _, c := range spy.Calls() {
		if c.Scope.IsZero() {
			t.Errorf("%s received a zero scope", c.Method)
		}
		if c.Scope.Canonical() != want.Canonical() {
			t.Errorf("%s got scope %q, want %q", c.Method, c.Scope.Canonical(), want.Canonical())
		}
	}
}

// TestRunAgent_ToolCallCarriesScope closes the last gap in the guard:
// StaticLLM never requests a tool, so CreateToolCall — implemented by the
// spy since Task 8 — was never actually exercised by any test. ToolCallingLLM
// forces one tool dispatch so this test proves CreateToolCall carries scope
// too, not just that the method exists on the spy.
func TestRunAgent_ToolCallCarriesScope(t *testing.T) {
	spy := scopespy.New()
	const toolName = "spy-tool"
	e, err := engine.New(
		engine.WithStore(spy),
		engine.WithLLM(scopespy.ToolCallingLLM(toolName)),
		engine.WithTool(
			llm.Tool{Name: toolName, Description: "test-only tool for scope coverage"},
			func(_ context.Context, _ string) (string, error) { return "ok", nil },
		),
	)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	want := cortex.Scope{Levels: []cortex.Level{
		{Key: "workspace", Value: "ws_x"},
		{Key: "project", Value: "proj_y"},
	}}
	ctx := cortex.WithScope(context.Background(), want)

	if _, err := e.RunAgent(ctx, "app1", "assistant", "hello", nil); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	sawToolCall := false
	for _, c := range spy.Calls() {
		if c.Scope.IsZero() {
			t.Errorf("%s received a zero scope", c.Method)
		}
		if c.Scope.Canonical() != want.Canonical() {
			t.Errorf("%s got scope %q, want %q", c.Method, c.Scope.Canonical(), want.Canonical())
		}
		if c.Method == "CreateToolCall" {
			sawToolCall = true
		}
	}
	if !sawToolCall {
		t.Fatal("CreateToolCall was never recorded; this test proves nothing about tool-call scope")
	}
}
