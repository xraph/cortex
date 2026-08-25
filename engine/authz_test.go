package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/knowledge"
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
	mu               sync.Mutex
	visible          []llm.Tool
	authorizeErr     error
	authorizeCalls   int
	visibleCalls     int
	visibleSubject   cortex.Subject
	authorizeSubject cortex.Subject
}

func (r *recordingAuthorizer) Visible(_ context.Context, s cortex.Subject, _ []llm.Tool) []llm.Tool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.visibleCalls++
	r.visibleSubject = s
	return r.visible
}

func (r *recordingAuthorizer) Authorize(_ context.Context, s cortex.Subject, _ llm.ToolCall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.authorizeCalls++
	r.authorizeSubject = s
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

// TestResolveTools_HonoursCfgToolsNames covers the resolveTools side of
// tool-name filtering: when cfg.Tools names a subset, only those tools
// are returned, and tools left out never reach the model regardless of
// what the authorizer would have allowed.
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

	got := e.resolveTools(context.Background(), cortex.Subject{}, []string{toolB.Name}, false)
	if len(got) != 1 || got[0].Name != toolB.Name {
		t.Fatalf("resolveTools with names=[%q] = %v, want only %q", toolB.Name, got, toolB.Name)
	}
}

// TestNilAuthorizer_LeavesBothPathsPermissive covers the "no authorizer
// set" side of the same guard: WithToolAuthorizer is never called, so
// both resolveTools (Visible) and executeTool (Authorize) must behave
// exactly as they did before the authorizer seam existed.
func TestNilAuthorizer_LeavesBothPathsPermissive(t *testing.T) {
	def, h := echoTool()
	e, err := New(WithTool(def, h))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tools := e.resolveTools(context.Background(), cortex.Subject{}, nil, false)
	var found bool
	for _, tl := range tools {
		if tl.Name == def.Name {
			found = true
		}
	}
	if !found {
		t.Fatalf("resolveTools with a nil authorizer dropped %q; a nil authorizer must allow everything", def.Name)
	}

	got, _, _ := e.executeTool(context.Background(), cortex.Subject{}, llm.ToolCall{Name: def.Name, Arguments: `{"x":1}`})
	if got != `echoed:{"x":1}` {
		t.Fatalf("executeTool with a nil authorizer = %q, want the handler's result unmodified", got)
	}
}

// TestExecuteTool_DispatchesWhenAuthorizerAllows is the missing permissive
// case for a real (non-nil) authorizer: every existing test that installs
// one has it deny. This proves Authorize returning nil actually lets
// dispatch through to the handler rather than the gate accidentally
// denying-by-default or the handler path being unreachable once an
// authorizer exists at all.
func TestExecuteTool_DispatchesWhenAuthorizerAllows(t *testing.T) {
	def, h := echoTool()
	authz := &recordingAuthorizer{visible: []llm.Tool{def}, authorizeErr: nil}
	e, err := New(WithTool(def, h), WithToolAuthorizer(authz))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, _, _ := e.executeTool(context.Background(), cortex.Subject{}, llm.ToolCall{Name: def.Name, Arguments: `{"x":1}`})
	if got != `echoed:{"x":1}` {
		t.Fatalf("executeTool with a permissive authorizer = %q, want the handler's result unmodified", got)
	}
	if authz.authorizeCalls != 1 {
		t.Errorf("Authorize called %d times, want 1", authz.authorizeCalls)
	}
}

// TestPrincipalFromContext_ReachesAuthorizerUnchanged is the regression
// guard for the gap the fix-round review caught: cortex.Subject.Principal
// existed but nothing could ever populate it, since none of the three
// Subject construction sites read it from anywhere. WithPrincipal/
// PrincipalFromContext close that gap; this proves a principal attached
// to ctx reaches both authorizer methods, and specifically that cortex
// carries it rather than copying or reinterpreting it: putting a pointer
// in and getting the identical pointer back out is the only way to prove
// "never interpreted" rather than merely "roughly preserved."
func TestPrincipalFromContext_ReachesAuthorizerUnchanged(t *testing.T) {
	type callerIdentity struct{ id string }
	principal := &callerIdentity{id: "user-42"}

	authz := &recordingAuthorizer{visible: []llm.Tool{{Name: "public"}}, authorizeErr: nil}
	e := newEngineWithTools(t, authz, llm.Tool{Name: "public"})

	ctx := cortex.WithPrincipal(context.Background(), principal)
	ctx = cortex.WithScope(ctx, cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
	if _, err := e.RunAgent(ctx, "assistant", "call public", nil); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	gotVisible, ok := authz.visibleSubject.Principal.(*callerIdentity)
	if !ok || gotVisible != principal {
		t.Errorf("Visible saw Principal %#v, want the identical pointer %p", authz.visibleSubject.Principal, principal)
	}
	gotAuthorize, ok := authz.authorizeSubject.Principal.(*callerIdentity)
	if !ok || gotAuthorize != principal {
		t.Errorf("Authorize saw Principal %#v, want the identical pointer %p", authz.authorizeSubject.Principal, principal)
	}
}

// TestPrincipalFromContext_AbsentReturnsNil mirrors ScopeFromContext's
// zero-value-on-absence contract: a context nobody attached a principal
// to must not panic, and must yield nil rather than some other zero-ish
// stand-in a caller might mistake for "no host principal configured" vs.
// "host configured principal, and it happens to be nil".
func TestPrincipalFromContext_AbsentReturnsNil(t *testing.T) {
	if got := cortex.PrincipalFromContext(context.Background()); got != nil {
		t.Errorf("PrincipalFromContext on a bare context = %#v, want nil", got)
	}
}

// stubKnowledge is a knowledge.Provider that answers nothing. Its only
// job is to be non-nil, since that is what makes the engine advertise the
// knowledge_search builtin.
type stubKnowledge struct{}

func (stubKnowledge) Retrieve(_ context.Context, _ string, _ *knowledge.RetrieveParams) ([]knowledge.ScoredChunk, error) {
	return nil, nil
}

func (stubKnowledge) ListCollections(_ context.Context) ([]knowledge.CollectionInfo, error) {
	return nil, nil
}

// TestResolveTools_BuiltinsSurviveCfgToolsFiltering pins the boundary
// cfg.Tools actually draws. It enumerates registered tools, not builtins:
// an agent that has Knowledge configured and also names a registered tool
// must keep knowledge_search, or its knowledge configuration becomes dead
// weight it has no way to notice. Withholding a builtin is the
// authorizer's job, not cfg.Tools'.
func TestResolveTools_BuiltinsSurviveCfgToolsFiltering(t *testing.T) {
	toolA, handlerA := echoTool()
	toolB := llm.Tool{Name: "other", Description: "a second tool"}
	handlerB := func(_ context.Context, _ cortex.Invocation) (string, error) { return "b", nil }

	e, err := New(
		WithKnowledge(stubKnowledge{}),
		WithTool(toolA, handlerA),
		WithTool(toolB, handlerB),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := e.resolveTools(context.Background(), cortex.Subject{}, []string{toolB.Name}, false)

	names := make([]string, 0, len(got))
	for _, tl := range got {
		names = append(names, tl.Name)
	}
	if len(names) != 2 {
		t.Fatalf("resolveTools = %v, want exactly [knowledge_search %s]", names, toolB.Name)
	}
	var sawBuiltin, sawNamed bool
	for _, n := range names {
		switch n {
		case "knowledge_search":
			sawBuiltin = true
		case toolB.Name:
			sawNamed = true
		case toolA.Name:
			t.Errorf("resolveTools returned %q, which cfg.Tools did not name", toolA.Name)
		}
	}
	if !sawBuiltin {
		t.Errorf("resolveTools = %v, dropped the knowledge_search builtin; cfg.Tools must not filter builtins", names)
	}
	if !sawNamed {
		t.Errorf("resolveTools = %v, dropped the registered tool cfg.Tools named", names)
	}
}
