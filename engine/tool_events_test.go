package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/store/scopespy"
)

// toolEventRecorder implements every tool hook cortex fires and records
// them in order. Recording all four is the point: a test that only
// watched ToolDenied could not tell whether ToolCompleted fired for the
// same call as well, which is exactly the bug these tests pin down.
type toolEventRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *toolEventRecorder) Name() string { return "tool-event-recorder" }

func (r *toolEventRecorder) record(name string) {
	r.mu.Lock()
	r.events = append(r.events, name)
	r.mu.Unlock()
}

func (r *toolEventRecorder) OnToolCalled(_ context.Context, _ id.AgentRunID, _ string, _ any) error {
	r.record("called")
	return nil
}

func (r *toolEventRecorder) OnToolCompleted(_ context.Context, _ id.AgentRunID, _, _ string, _ time.Duration) error {
	r.record("completed")
	return nil
}

func (r *toolEventRecorder) OnToolFailed(_ context.Context, _ id.AgentRunID, _ string, _ error) error {
	r.record("failed")
	return nil
}

func (r *toolEventRecorder) OnToolDenied(_ context.Context, _ id.AgentRunID, _, _ string) error {
	r.record("denied")
	return nil
}

func (r *toolEventRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// countEvent returns how many times name was recorded.
func countEvent(events []string, name string) int {
	n := 0
	for _, e := range events {
		if e == name {
			n++
		}
	}
	return n
}

// runToolCallWithRecorder drives one agent run that dispatches a single
// tool call and returns every tool event the run produced. The handler
// and authorizer are the knobs each test turns.
func runToolCallWithRecorder(t *testing.T, authorizer cortex.ToolAuthorizer, handler ToolHandler) []string {
	t.Helper()

	rec := &toolEventRecorder{}
	def := llm.Tool{Name: "probe", Description: "test-only tool"}
	e, err := New(
		WithStore(scopespy.New()),
		WithLLM(scopespy.ToolCallingLLM(def.Name)),
		WithTool(def, handler),
		WithToolAuthorizer(authorizer),
		WithExtension(rec),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
	if _, err := e.RunAgent(ctx, "assistant", "call probe", nil); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	events := rec.snapshot()
	if countEvent(events, "called") != 1 {
		t.Fatalf("ToolCalled fired %d times, want 1; the run did not dispatch exactly one tool call: %v", countEvent(events, "called"), events)
	}
	return events
}

func okHandler(_ context.Context, _ cortex.Invocation) (string, error) { return "handler ran", nil }

// TestToolEvents_DeniedFiresDeniedOnly is the core of the one-terminal-
// event invariant. The loop used to emit ToolCompleted unconditionally
// after executeTool returned, so a denial fired denied-then-completed and
// every audit subscriber counting completions counted the denial as a
// success.
func TestToolEvents_DeniedFiresDeniedOnly(t *testing.T) {
	authz := &recordingAuthorizer{visible: nil, authorizeErr: errors.New("not permitted")}

	events := runToolCallWithRecorder(t, authz, okHandler)

	if got := countEvent(events, "denied"); got != 1 {
		t.Errorf("ToolDenied fired %d times, want 1: %v", got, events)
	}
	if got := countEvent(events, "completed"); got != 0 {
		t.Errorf("ToolCompleted fired %d times on a denied call, want 0: %v", got, events)
	}
	if got := countEvent(events, "failed"); got != 0 {
		t.Errorf("ToolFailed fired %d times on a denied call, want 0: %v", got, events)
	}
}

// TestToolEvents_HandlerErrorFiresFailedOnly is the failure half of the
// same invariant: plugin.ToolFailed started firing in this release, and
// the unconditional ToolCompleted turned every failure into a reported
// success alongside it.
func TestToolEvents_HandlerErrorFiresFailedOnly(t *testing.T) {
	boom := func(_ context.Context, _ cortex.Invocation) (string, error) {
		return "", errors.New("handler exploded")
	}

	events := runToolCallWithRecorder(t, nil, boom)

	if got := countEvent(events, "failed"); got != 1 {
		t.Errorf("ToolFailed fired %d times, want 1: %v", got, events)
	}
	if got := countEvent(events, "completed"); got != 0 {
		t.Errorf("ToolCompleted fired %d times on a failed call, want 0: %v", got, events)
	}
	if got := countEvent(events, "denied"); got != 0 {
		t.Errorf("ToolDenied fired %d times on a failed call, want 0: %v", got, events)
	}
}

// TestToolEvents_SuccessFiresCompletedOnce guards the other direction:
// narrowing when ToolCompleted fires must not stop it firing on the
// ordinary path.
func TestToolEvents_SuccessFiresCompletedOnce(t *testing.T) {
	events := runToolCallWithRecorder(t, nil, okHandler)

	if got := countEvent(events, "completed"); got != 1 {
		t.Errorf("ToolCompleted fired %d times on a successful call, want 1: %v", got, events)
	}
	if got := countEvent(events, "failed"); got != 0 {
		t.Errorf("ToolFailed fired %d times on a successful call, want 0: %v", got, events)
	}
	if got := countEvent(events, "denied"); got != 0 {
		t.Errorf("ToolDenied fired %d times on a successful call, want 0: %v", got, events)
	}
}

// TestToolEvents_StreamingDeniedFiresDeniedOnly covers the second copy of
// the tool-dispatch loop. streamReAct emitted ToolCompleted
// unconditionally too, and a fix applied to only one of the two loops
// would leave streaming hosts with the same miscount.
func TestToolEvents_StreamingDeniedFiresDeniedOnly(t *testing.T) {
	rec := &toolEventRecorder{}
	def := llm.Tool{Name: "probe", Description: "test-only tool"}
	authz := &recordingAuthorizer{visible: nil, authorizeErr: errors.New("not permitted")}

	e, err := New(
		WithStore(scopespy.New()),
		WithLLM(scopespy.ToolCallingStreamLLM(def.Name)),
		WithTool(def, okHandler),
		WithToolAuthorizer(authz),
		WithExtension(rec),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
	events := make(chan StreamEvent, 64)
	if err := e.StreamAgent(ctx, "assistant", "call probe", nil, events); err != nil {
		t.Fatalf("StreamAgent: %v", err)
	}
	// streamReAct closes the channel only after its goroutine has finished
	// every emit, so draining is the synchronisation point.
	drained := 0
	for range events {
		drained++
	}
	if drained == 0 {
		t.Fatal("the stream produced no events at all; nothing was synchronised on")
	}

	got := rec.snapshot()
	if n := countEvent(got, "called"); n != 1 {
		t.Fatalf("ToolCalled fired %d times, want 1; the stream did not dispatch one tool call: %v", n, got)
	}
	if n := countEvent(got, "denied"); n != 1 {
		t.Errorf("ToolDenied fired %d times, want 1: %v", n, got)
	}
	if n := countEvent(got, "completed"); n != 0 {
		t.Errorf("ToolCompleted fired %d times on a denied streaming call, want 0: %v", n, got)
	}
}

// TestExecuteTool_UnknownToolFiresFailed pins the outcome of a name that
// matches nothing. It used to fire no terminal event at all through
// Dispatch and a bare ToolCompleted through the loop, which reported a
// tool that never ran as a success.
func TestExecuteTool_UnknownToolFiresFailed(t *testing.T) {
	rec := &toolEventRecorder{}
	e, err := New(WithExtension(rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, outcome := e.executeTool(context.Background(), cortex.Subject{}, llm.ToolCall{Name: "nope"})

	if outcome != outcomeFailed {
		t.Errorf("outcome = %d, want outcomeFailed (%d)", outcome, outcomeFailed)
	}
	if got := countEvent(rec.snapshot(), "failed"); got != 1 {
		t.Errorf("ToolFailed fired %d times for an unknown tool, want 1: %v", got, rec.snapshot())
	}
	if result == "" {
		t.Error("executeTool returned an empty result for an unknown tool; the model needs something to read")
	}
}
