package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/store/scopespy"
)

// RunAgent/StreamAgent dropped their appID parameter this phase: agent
// lookups are scope-guarded now, so app_id no longer does anything for
// them. cortex.WithApp/AppFromContext are gone entirely as of the
// breaking sweep: agents, skills, traits, behaviors, personas, and
// orchestration are all scope-guarded now, and a host wanting an app
// dimension declares it as a scope level instead.

func TestRunAgent_RejectsZeroScope(t *testing.T) {
	spy := scopespy.New()
	e, err := engine.New(engine.WithStore(spy))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	_, err = e.RunAgent(context.Background(), "assistant", "hello", nil)
	if !errors.Is(err, cortex.ErrNoScope) {
		t.Fatalf("err = %v, want ErrNoScope", err)
	}
	if spy.CallCount() != 0 {
		t.Errorf("store was called %d times on a zero scope; want 0", spy.CallCount())
	}
}

// TestRunAgent_EveryStoreCallCarriesScope is the regression guard for the
// cross-tenant conversation bleed. The react loop used to pass "" as the
// tenant on all four conversation calls, so every tenant shared one
// history bucket. The spy fails the test if any recorded call arrives
// with a zero scope, which is what a future forgotten call site looks like.
func TestRunAgent_EveryStoreCallCarriesScope(t *testing.T) {
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

	if _, err := e.RunAgent(ctx, "assistant", "hello", nil); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	if spy.CallCount() == 0 {
		t.Fatal("spy recorded no store calls; the test proves nothing")
	}
	var sawSaveConversation bool
	for _, c := range spy.Calls() {
		if c.Scope.IsZero() {
			t.Errorf("%s received a zero scope", c.Method)
		}
		if c.Scope.Canonical() != want.Canonical() {
			t.Errorf("%s got scope %q, want %q", c.Method, c.Scope.Canonical(), want.Canonical())
		}
		// resolveSession runs once at the top of runReAct and its result
		// is threaded to every conversation call. A zero id here would
		// mean either the override plumbing broke or resolveSession
		// silently failed to produce a real default session.
		if c.Method == "SaveConversation" {
			sawSaveConversation = true
			if c.SessionID.IsNil() {
				t.Error("SaveConversation received a zero session id; want the resolved default session")
			}
		}
	}
	if !sawSaveConversation {
		t.Fatal("SaveConversation was never recorded; this test proves nothing about session resolution")
	}
}

// TestStreamAgent_EveryStoreCallCarriesScope is the streaming sibling of
// TestRunAgent_EveryStoreCallCarriesScope. streamReAct does its store work
// (LoadConversation, CreateStep, SaveConversation, UpdateRun) on a
// goroutine that TestRunAgent_EveryStoreCallCarriesScope never exercises — a
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
	if err := e.StreamAgent(ctx, "assistant", "hello", nil, events); err != nil {
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
// StaticLLM never requests a tool, so CreateToolCall, implemented on the
// spy, was never actually exercised by any test. ToolCallingLLM forces
// one tool dispatch so this test proves CreateToolCall carries scope
// too, not just that the method exists on the spy.
func TestRunAgent_ToolCallCarriesScope(t *testing.T) {
	spy := scopespy.New()
	const toolName = "spy-tool"
	e, err := engine.New(
		engine.WithStore(spy),
		engine.WithLLM(scopespy.ToolCallingLLM(toolName)),
		engine.WithTool(
			llm.Tool{Name: toolName, Description: "test-only tool for scope coverage"},
			func(_ context.Context, _ cortex.Invocation) (string, error) { return "ok", nil },
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

	if _, err := e.RunAgent(ctx, "assistant", "hello", nil); err != nil {
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

// TestStreamAgent_CancelPersistsWithUncancelledContext is a regression
// test for the terminal cancel write: the cancel branch in streamReAct
// used to call UpdateRun with the ctx that had just been cancelled, so
// the store write failed before it started and the run stayed "running"
// forever instead of recording StateCancelled. BlockingStreamLLM hangs
// in Next until the test cancels ctx, forcing the loop back to its
// ctx.Done() branch deterministically. The scope must still ride along
// even though cancellation was stripped for the write.
func TestStreamAgent_CancelPersistsWithUncancelledContext(t *testing.T) {
	spy := scopespy.New()
	e, err := engine.New(engine.WithStore(spy), engine.WithLLM(scopespy.BlockingStreamLLM()))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	want := cortex.Scope{Levels: []cortex.Level{
		{Key: "workspace", Value: "ws_x"},
		{Key: "project", Value: "proj_y"},
	}}
	baseCtx := cortex.WithScope(context.Background(), want)
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()

	events := make(chan engine.StreamEvent, 64)
	if err := e.StreamAgent(ctx, "assistant", "hello", nil, events); err != nil {
		t.Fatalf("StreamAgent: %v", err)
	}

	cancel()

	var drained int
	for range events {
		drained++
	}
	if drained == 0 {
		t.Fatal("no stream events received; StreamAgent may not have run")
	}

	var sawUpdate bool
	for _, c := range spy.Calls() {
		if c.Method != "UpdateRun" {
			continue
		}
		sawUpdate = true
		if c.Scope.IsZero() {
			t.Error("UpdateRun on cancel received a zero scope")
		}
		if c.Scope.Canonical() != want.Canonical() {
			t.Errorf("UpdateRun on cancel got scope %q, want %q", c.Scope.Canonical(), want.Canonical())
		}
		if c.CtxErr != nil {
			t.Errorf("UpdateRun on cancel got ctx.Err() = %v, want nil (the write must use context.WithoutCancel)", c.CtxErr)
		}
	}
	if !sawUpdate {
		t.Fatal("UpdateRun was never recorded on cancel; the cancel branch did not run")
	}
}

// TestStreamAgent_MockCancelPersistsWithUncancelledContext is the
// streamMock sibling of TestStreamAgent_CancelPersistsWithUncancelledContext:
// the mock/echo fallback (used when no LLM client is configured) has its
// own independent cancel branch with the same bug shape. It emits one
// token per character with a short sleep between them, so cancelling
// right after the first token gives the loop's next ctx.Done() check a
// wide, deterministic window to fire.
func TestStreamAgent_MockCancelPersistsWithUncancelledContext(t *testing.T) {
	spy := scopespy.New()
	e, err := engine.New(engine.WithStore(spy)) // no WithLLM: forces the mock/echo fallback
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	want := cortex.Scope{Levels: []cortex.Level{
		{Key: "workspace", Value: "ws_x"},
		{Key: "project", Value: "proj_y"},
	}}
	baseCtx := cortex.WithScope(context.Background(), want)
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()

	events := make(chan engine.StreamEvent, 64)
	if err := e.StreamAgent(ctx, "assistant", "hello", nil, events); err != nil {
		t.Fatalf("StreamAgent: %v", err)
	}

	var gotToken bool
	for ev := range events {
		if ev.Type == engine.EventToken && !gotToken {
			gotToken = true
			cancel()
		}
	}
	if !gotToken {
		t.Fatal("no token event received; the mock fallback may not have run")
	}

	var sawUpdate bool
	for _, c := range spy.Calls() {
		if c.Method != "UpdateRun" {
			continue
		}
		sawUpdate = true
		if c.Scope.IsZero() {
			t.Error("UpdateRun on cancel received a zero scope")
		}
		if c.Scope.Canonical() != want.Canonical() {
			t.Errorf("UpdateRun on cancel got scope %q, want %q", c.Scope.Canonical(), want.Canonical())
		}
		if c.CtxErr != nil {
			t.Errorf("UpdateRun on cancel got ctx.Err() = %v, want nil (the write must use context.WithoutCancel)", c.CtxErr)
		}
	}
	if !sawUpdate {
		t.Fatal("UpdateRun was never recorded on cancel; the cancel branch did not run")
	}
}

// failingPersonaSpy wraps scopespy.Spy but always fails persona
// resolution, simulating a persona lookup that finds nothing at the
// caller's scope. scopespy.Spy's own GetPersonaByName always succeeds,
// so it can't reproduce this on its own.
type failingPersonaSpy struct {
	*scopespy.Spy
}

func (f *failingPersonaSpy) GetPersonaByName(_ context.Context, _ string) (*persona.Persona, error) {
	return nil, cortex.ErrPersonaNotFound
}

// TestRunAgent_PersonaLookupFailureAbortsLoudly is a regression guard:
// BuildSystemPrompt must abort the run when persona resolution fails,
// not silently drop the Identity section from the prompt. This test
// used to be named for a "missing app" scenario: persona lookup used to
// key on cortex.AppFromContext(ctx), and a context with no app attached
// reproduced the failure directly. Persona lookup is scope-keyed now,
// and RunAgent's own guard already rejects a context with no scope
// before BuildSystemPrompt ever runs, so there is no missing-app path
// left that reaches this code -- the spy above fails the lookup outright
// instead, to keep exercising the same fail-loud invariant under the
// new signature.
func TestRunAgent_PersonaLookupFailureAbortsLoudly(t *testing.T) {
	spy := &failingPersonaSpy{Spy: scopespy.New()}
	e, err := engine.New(engine.WithStore(spy), engine.WithLLM(scopespy.StaticLLM("done")))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})

	if _, err := e.RunAgent(ctx, "assistant", "hello", nil); err == nil {
		t.Fatal("RunAgent with a failing persona lookup succeeded despite the agent requesting a persona; it must fail instead of silently dropping the Identity section from the prompt")
	} else if !errors.Is(err, cortex.ErrPersonaNotFound) {
		t.Errorf("RunAgent err = %v, want it to wrap cortex.ErrPersonaNotFound", err)
	}

	if spy.CallCount() == 0 {
		t.Fatal("spy recorded no store calls; GetByName never ran, so this test proves nothing about persona resolution")
	}
}
