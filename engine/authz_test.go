package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/scopespy"
)

// deniedToolRecorder records every OnToolDenied call it receives.
type deniedToolRecorder struct {
	calls []string
}

func (r *deniedToolRecorder) Name() string { return "denied-tool-recorder" }

func (r *deniedToolRecorder) OnToolDenied(_ context.Context, _ id.AgentRunID, toolName, _ string) error {
	r.calls = append(r.calls, toolName)
	return nil
}

// TestEmitToolDenied_RecordsExactlyOneDenial predates the authorizer
// wiring below: it proves the plugin.ToolDenied hook itself fires and
// carries the right tool name, independent of anything that calls it.
func TestEmitToolDenied_RecordsExactlyOneDenial(t *testing.T) {
	rec := &deniedToolRecorder{}
	e, err := New(WithExtension(rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	e.Extensions().EmitToolDenied(context.Background(), id.NewAgentRunID(), "delete_everything", "not authorized")

	if len(rec.calls) != 1 {
		t.Fatalf("OnToolDenied called %d times, want 1", len(rec.calls))
	}
	if rec.calls[0] != "delete_everything" {
		t.Errorf("denied tool = %q, want %q", rec.calls[0], "delete_everything")
	}
}

// recordingAuthorizer is a ToolAuthorizer double that returns a fixed
// Visible list (nil hides every tool) and a fixed Authorize error, while
// counting how many times each method ran. That count is what proves
// Authorize was actually consulted rather than Visible's filtering being
// trusted in its place.
type recordingAuthorizer struct {
	mu             sync.Mutex
	visible        []llm.Tool
	authorizeErr   error
	authorizeCalls int
	visibleCalls   int
}

func (r *recordingAuthorizer) Visible(_ context.Context, _ cortex.Subject, _ []llm.Tool) []llm.Tool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.visibleCalls++
	return r.visible
}

func (r *recordingAuthorizer) Authorize(_ context.Context, _ cortex.Subject, _ llm.ToolCall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.authorizeCalls++
	return r.authorizeErr
}

// capturingToolCallSpy wraps scopespy.Spy to also capture each ToolCall's
// Result. The base Spy records that CreateToolCall happened but discards
// the struct it was given, and runOneToolCall needs the raw result string
// a denial produces, not just proof the call was persisted.
type capturingToolCallSpy struct {
	*scopespy.Spy
	mu      sync.Mutex
	results []string
}

func (c *capturingToolCallSpy) CreateToolCall(ctx context.Context, tc *run.ToolCall) error {
	c.mu.Lock()
	c.results = append(c.results, tc.Result)
	c.mu.Unlock()
	return c.Spy.CreateToolCall(ctx, tc)
}

// newEngineWithTools builds an engine wired with authorizer, a
// ToolCallingLLM double that calls tools[0] by name on its first step,
// and a no-op handler for every tool passed in. The handler's result is
// distinctive so a test can tell whether it actually ran.
func newEngineWithTools(t *testing.T, authorizer cortex.ToolAuthorizer, tools ...llm.Tool) *Engine {
	t.Helper()
	if len(tools) == 0 {
		t.Fatal("newEngineWithTools requires at least one tool")
	}

	spy := &capturingToolCallSpy{Spy: scopespy.New()}
	opts := make([]Option, 0, 3+len(tools))
	opts = append(opts,
		WithStore(spy),
		WithLLM(scopespy.ToolCallingLLM(tools[0].Name)),
		WithToolAuthorizer(authorizer),
	)
	for _, tl := range tools {
		opts = append(opts, WithTool(tl, func(_ context.Context, _ cortex.Invocation) (string, error) {
			return "handler executed", nil
		}))
	}

	e, err := New(opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// runOneToolCall runs the engine's single test agent to completion and
// returns the result of the first (and, for these tests, only) tool call
// it dispatched.
func runOneToolCall(t *testing.T, e *Engine, toolName string) string {
	t.Helper()
	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})

	if _, err := e.RunAgent(ctx, "assistant", "call "+toolName, nil); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	spy, ok := e.Store().(*capturingToolCallSpy)
	if !ok {
		t.Fatalf("engine store is %T, want *capturingToolCallSpy", e.Store())
	}
	if len(spy.results) == 0 {
		t.Fatal("no tool call was recorded; runOneToolCall proves nothing")
	}
	return spy.results[len(spy.results)-1]
}

// TestExecuteTool_DeniesAToolThatWasNeverVisible is the test that
// matters most: a model can name a tool it was never shown, so Visible
// filtering the list handed to the LLM is not a substitute for gating
// the dispatch itself. The authorizer here hides every tool from Visible
// and still must be asked by Authorize before the hidden tool runs.
func TestExecuteTool_DeniesAToolThatWasNeverVisible(t *testing.T) {
	spy := &recordingAuthorizer{visible: nil, authorizeErr: errors.New("not permitted")}
	e := newEngineWithTools(t, spy, llm.Tool{Name: "secret"})

	// The LLM double names a tool the authorizer hid.
	out := runOneToolCall(t, e, "secret")

	if !strings.Contains(out, "not permitted") {
		t.Errorf("dispatching a hidden tool produced %q; the denial must reach the model", out)
	}
	if strings.Contains(out, "handler executed") {
		t.Errorf("dispatching a hidden tool produced %q; the handler must not run on a denial", out)
	}
	if spy.authorizeCalls != 1 {
		t.Errorf("Authorize called %d times, want 1: filtering must not be trusted in place of gating", spy.authorizeCalls)
	}
	if spy.visibleCalls == 0 {
		t.Error("Visible was never called; this test proves nothing about the list the model saw")
	}
}

// TestResolveTools_HonoursCfgToolsNames is the resolveTools half of Task
// 3: when cfg.Tools names a subset, only those tools are returned, and
// tools left out never reach the model regardless of what the authorizer
// would have allowed.
func TestResolveTools_HonoursCfgToolsNames(t *testing.T) {
	toolA, _ := echoTool()
	toolB := llm.Tool{Name: "other", Description: "a second tool"}
	handlerB := func(_ context.Context, _ cortex.Invocation) (string, error) { return "b", nil }

	e, err := New(WithTool(toolA, func(_ context.Context, inv cortex.Invocation) (string, error) {
		return "echoed:" + inv.Call.Arguments, nil
	}), WithTool(toolB, handlerB))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := e.resolveTools(context.Background(), cortex.Subject{}, []string{toolB.Name})
	if len(got) != 1 || got[0].Name != toolB.Name {
		t.Fatalf("resolveTools with names=[%q] = %v, want only %q", toolB.Name, got, toolB.Name)
	}
}

// TestNilAuthorizer_LeavesBothPathsPermissive is the "no authorizer set"
// half of Task 3: WithToolAuthorizer is never called, so both resolveTools
// (Visible) and executeTool (Authorize) must behave exactly as they did
// before the authorizer seam existed.
func TestNilAuthorizer_LeavesBothPathsPermissive(t *testing.T) {
	def, h := echoTool()
	e, err := New(WithTool(def, h))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tools := e.resolveTools(context.Background(), cortex.Subject{}, nil)
	var found bool
	for _, tl := range tools {
		if tl.Name == def.Name {
			found = true
		}
	}
	if !found {
		t.Fatalf("resolveTools with a nil authorizer dropped %q; a nil authorizer must allow everything", def.Name)
	}

	got := e.executeTool(context.Background(), cortex.Subject{}, llm.ToolCall{Name: def.Name, Arguments: `{"x":1}`})
	if got != `echoed:{"x":1}` {
		t.Fatalf("executeTool with a nil authorizer = %q, want the handler's result unmodified", got)
	}
}
