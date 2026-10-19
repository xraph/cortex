package engine

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/telemetry/telemetrytest"
	"github.com/xraph/cortex/knowledge"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/store/scopespy"
	"github.com/xraph/cortex/suspension"
)

// chunkStream replays fixed chunks. onNext, when set, runs before each
// chunk and can fail the read.
type chunkStream struct {
	chunks []*llm.Chunk
	i      int
	onNext func(context.Context) error
}

func (s *chunkStream) Next(ctx context.Context) (*llm.Chunk, error) {
	if s.onNext != nil {
		if err := s.onNext(ctx); err != nil {
			return nil, err
		}
	}
	if s.i >= len(s.chunks) {
		return nil, io.EOF
	}
	c := s.chunks[s.i]
	s.i++
	return c, nil
}

func (s *chunkStream) Close() error      { return nil }
func (s *chunkStream) Usage() *llm.Usage { return &llm.Usage{TotalTokens: 3} }

// streamingLLM hands out its streams in order, then plain answers.
type streamingLLM struct {
	mu      sync.Mutex
	streams []*chunkStream
}

func (m *streamingLLM) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	return nil, errors.New("streamingLLM: streaming only")
}

func (m *streamingLLM) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.streams) == 0 {
		return &chunkStream{chunks: []*llm.Chunk{{Content: "done", FinishReason: "stop"}}}, nil
	}
	s := m.streams[0]
	m.streams = m.streams[1:]
	return s, nil
}

func assertNoCanaries(t *testing.T, rec *tracetest.SpanRecorder, sink *telemetrytest.Sink, canaries []string) {
	t.Helper()
	for _, s := range rec.Ended() {
		text := spanText(s)
		for _, c := range canaries {
			if strings.Contains(text, c) {
				t.Errorf("span %q (scope %s) carries %q:\n%s", s.Name(), s.InstrumentationScope().Name, c, text)
			}
		}
	}
	if sink == nil {
		return
	}
	scrape := sink.Scrape()
	for _, c := range canaries {
		if strings.Contains(scrape, c) {
			t.Errorf("the metric scrape carries %q", c)
		}
	}
}

// The streaming loop, with loaded history and a scope value of its own,
// keeps every chunk, history message and scope value off spans and labels.
func TestStreamingTelemetryCarriesNoUserContent(t *testing.T) {
	rec := recordSpans(t)
	sink := telemetrytest.NewSink()
	gate := &approvingGate{}
	model := &streamingLLM{streams: []*chunkStream{
		{chunks: []*llm.Chunk{
			{Content: "CHUNK-CANARY looking up alice@example.com "},
			{ToolCalls: []llm.ToolCall{{ID: "call-9", Name: "lookup_customer", Arguments: `{"email":"alice@example.com","note":"ARG-CANARY"}`}}, FinishReason: "tool_calls"},
		}},
		{chunks: []*llm.Chunk{{Content: "final OUTPUT-CANARY CHUNK-CANARY", FinishReason: "stop"}}},
	}}
	st := &historySpy{Spy: scopespy.New(), history: []memory.Message{
		{Role: "user", Content: "HISTORY-CANARY earlier, from alice@example.com"},
		{Role: "assistant", Content: "HISTORY-CANARY earlier reply"},
	}}
	e, err := New(WithStore(st), WithLLM(model), WithMetrics(sink), WithPublicationGate(gate),
		WithTool(llm.Tool{Name: "lookup_customer", Description: "test-only"}, func(context.Context, cortex.Invocation) (string, error) {
			return `{"flag":"RESULT-CANARY","email":"alice@example.com"}`, nil
		}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "SCOPE-CANARY-tenant"}}})
	events := make(chan StreamEvent, 128)
	if err := e.StreamAgent(ctx, "secret-agent-name", "INPUT-CANARY alice@example.com", &RunOverrides{SystemPrompt: "PROMPT-CANARY"}, events); err != nil {
		t.Fatalf("StreamAgent: %v", err)
	}
	var last StreamEvent
	for ev := range events {
		last = ev
	}
	if last.Type != EventDone {
		t.Fatalf("last event = %v, want done", last)
	}
	for _, want := range []string{"HISTORY-CANARY", "CHUNK-CANARY", "ARG-CANARY", "RESULT-CANARY", "INPUT-CANARY"} {
		if !gate.saw(want) {
			t.Fatalf("the gate never saw %q", want)
		}
	}
	assertNoCanaries(t, rec, sink, append([]string{"SCOPE-CANARY", "HISTORY-CANARY", "CHUNK-CANARY"}, canaries...))

	spans := cortexSpans(rec)
	runs := spansNamed(spans, spanRunAgent)
	if len(runs) != 1 || attrString(runs[0], attrRunMode) != modeStream || attrString(runs[0], attrRunState) != "completed" {
		t.Fatalf("run agent spans = %v", runs)
	}
	var history bool
	for _, s := range spansNamed(spans, spanCheckPublication) {
		if attrString(s, attrPublicationSource) == "history" && attrString(s, attrPublicationChecks) == "2" {
			history = true
		}
	}
	if !history {
		t.Error("no history batch span for the two loaded messages")
	}
	if got := sink.Value(metricToolCallsHandled, map[string]string{"kind": "registered", "outcome": "completed"}); got != 1 {
		t.Errorf("tool calls = %v, want 1", got)
	}
}

// What one agent sends another, and the agents' names, stay off the
// delivery and run spans too.
func TestMessagingTelemetryCarriesNoUserContent(t *testing.T) {
	rec := recordSpans(t)
	sink := telemetrytest.NewSink()
	ctx := wsCtx()
	st := newApprovalStore(ctx, t)
	model := &twoStepLLM{
		calls: []llm.ToolCall{{ID: "call-1", Name: toolAgentSend,
			Arguments: `{"to":"secret-agent-name-worker","content":"A2A-CANARY tell alice@example.com"}`}},
		final: "sent",
	}
	e, err := New(WithStore(st), WithLLM(model), WithMetrics(sink), WithA2A(a2a.Options{HopCeiling: 6, Workers: 1}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, name := range []string{"secret-agent-name-planner", "secret-agent-name-worker"} {
		if createErr := e.CreateAgent(ctx, &agent.Config{ID: id.NewAgentID(), Name: name, Model: "test-model", MaxSteps: 4}); createErr != nil {
			t.Fatalf("CreateAgent: %v", createErr)
		}
	}
	if _, err := e.RunAgent(ctx, "secret-agent-name-planner", "pass it on", nil); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if _, err := e.A2A().Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	var delivered bool
	for _, s := range rec.Ended() {
		if s.Name() == "deliver agent message" && attrString(s, "cortex.a2a.class") == "informative" {
			delivered = true
		}
	}
	if !delivered {
		t.Fatal("the message was never delivered, so the test proves nothing")
	}
	assertNoCanaries(t, rec, sink, []string{"A2A-CANARY", "alice@example.com", "secret-agent-name"})
}

// A tool that panics still ends its span, its step and its run, and the
// in-flight gauge goes back to zero, before the panic reaches the caller.
func TestPanickingToolEndsItsSpans(t *testing.T) {
	rec := recordSpans(t)
	sink := telemetrytest.NewSink()
	e, err := New(
		WithStore(scopespy.New()),
		WithLLM(scopespy.ToolCallingLLM("explode")),
		WithMetrics(sink),
		WithTool(llm.Tool{Name: "explode", Description: "test-only"}, func(context.Context, cortex.Invocation) (string, error) {
			panic("tool blew up with alice@example.com")
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic did not reach the caller")
			}
		}()
		_, _ = e.RunAgent(wsCtx(), "assistant", "go", nil)
	}()

	if got := sink.Value(metricRunsInProgress, map[string]string{"mode": "sync"}); got != 0 {
		t.Errorf("runs in progress = %v after a panic, want 0", got)
	}
	if got := sink.Value(metricToolCallsHandled, map[string]string{"kind": "registered", "outcome": "failed"}); got != 1 {
		t.Errorf("failed tool calls = %v, want 1", got)
	}
	spans := cortexSpans(rec)
	for _, name := range []string{spanRunAgent, spanRunStep, spanCallTool} {
		got := spansNamed(spans, name)
		if len(got) != 1 {
			t.Fatalf("%d ended %q spans, want 1", len(got), name)
		}
		if got[0].Status().Code != codes.Error || got[0].Status().Description != "panic" {
			t.Errorf("%q status = %v, want the panic class", name, got[0].Status())
		}
	}
	assertNoCanaries(t, rec, sink, []string{"alice@example.com", "blew up"})
}

// cancellingLLM cancels the run's context mid-call and reports it, the way a
// provider does when the caller goes away.
type cancellingLLM struct{ cancel context.CancelFunc }

func (m *cancellingLLM) Complete(ctx context.Context, _ *llm.Request) (*llm.Response, error) {
	m.cancel()
	return nil, ctx.Err()
}

func (m *cancellingLLM) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return &chunkStream{onNext: func(ctx context.Context) error {
		m.cancel()
		return ctx.Err()
	}}, nil
}

// A cancellation during the model call stops the step; it does not fail
// it, on either loop.
func TestCancelledModelCallStopsTheStep(t *testing.T) {
	for _, mode := range []string{modeSync, modeStream} {
		t.Run(mode, func(t *testing.T) {
			rec := recordSpans(t)
			sink := telemetrytest.NewSink()
			ctx, cancel := context.WithCancel(wsCtx())
			defer cancel()
			e, err := New(WithStore(scopespy.New()), WithLLM(&cancellingLLM{cancel: cancel}), WithMetrics(sink))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if mode == modeSync {
				_, _ = e.RunAgent(ctx, "assistant", "go", nil)
			} else {
				events := make(chan StreamEvent, 16)
				if err := e.StreamAgent(ctx, "assistant", "go", nil, events); err != nil {
					t.Fatalf("StreamAgent: %v", err)
				}
				for ev := range events {
					_ = ev
				}
			}
			spans := cortexSpans(rec)
			steps := spansNamed(spans, spanRunStep)
			if len(steps) != 1 || attrString(steps[0], attrStepOutcome) != stepStopped || steps[0].Status().Code == codes.Error {
				t.Fatalf("step spans = %v, want one stopped step that is not an error", steps)
			}
			if got := sink.Value(metricStepsExecuted, map[string]string{"outcome": "error"}); got != 0 {
				t.Errorf("a cancellation counted as %v error step(s)", got)
			}
			runs := spansNamed(spans, spanRunAgent)
			if len(runs) != 1 || runs[0].Status().Code == codes.Error || attrString(runs[0], attrRunState) != "cancelled" {
				t.Errorf("run span = state %q, status %v; want a cancelled run that is not an error",
					attrString(runs[0], attrRunState), runs[0].Status())
			}
		})
	}
}

func TestDeadlineStopsAStep(t *testing.T) {
	rec := recordSpans(t)
	e, err := New(WithStore(scopespy.New()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, step := e.startStep(context.Background(), 0)
	step.finish(stepError, context.DeadlineExceeded)
	got := spansNamed(cortexSpans(rec), spanRunStep)
	if len(got) != 1 || attrString(got[0], attrStepOutcome) != stepStopped || got[0].Status().Code == codes.Error {
		t.Fatalf("a deadline step = %v, want stopped", got)
	}
}

// failingSuspensionStore cannot write the suspension row.
type failingSuspensionStore struct{ *scopespy.Spy }

func (failingSuspensionStore) CreateSuspension(context.Context, *suspension.Suspension) error {
	return errors.New("database is down")
}

// A store failure while pausing is counted under the reason the calls
// wanted, not as calls that disagreed.
func TestSuspendStoreFailureKeepsItsReason(t *testing.T) {
	rec := recordSpans(t)
	sink := telemetrytest.NewSink()
	def := externalTool()
	e, err := New(
		WithStore(failingSuspensionStore{Spy: scopespy.New()}),
		WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{{ID: "call-1", Name: def.Name, Arguments: "{}"}}}),
		WithExternalTool(def),
		WithMetrics(sink),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := e.RunAgent(wsCtx(), "assistant", "ask", nil); err == nil {
		t.Fatal("RunAgent succeeded against a store that cannot suspend")
	}
	if got := sink.Value(metricRunsSuspended, map[string]string{"reason": "external_tool", "result": "error"}); got != 1 {
		t.Errorf("suspend failures under external_tool = %v, want 1", got)
	}
	if got := sink.Value(metricRunsSuspended, map[string]string{"reason": "mixed", "result": "error"}); got != 0 {
		t.Errorf("a store failure was counted as mixed reasons")
	}
	s := spansNamed(cortexSpans(rec), spanSuspendRun)
	if len(s) != 1 || attrString(s[0], attrSuspensionReason) != "external_tool" || s[0].Status().Code != codes.Error {
		t.Fatalf("suspend run spans = %v", s)
	}
}

// Resuming a run that does not exist is an answer, not a fault.
func TestResumeOfAMissingRunIsNotAnError(t *testing.T) {
	rec := recordSpans(t)
	e, err := New(WithStore(scopespy.New()), WithLLM(&llm.MockClient{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := e.Resume(wsCtx(), id.NewAgentRunID(), ResumeInput{}); !errors.Is(err, cortex.ErrRunNotFound) {
		t.Fatalf("Resume = %v, want ErrRunNotFound", err)
	}
	runs := spansNamed(cortexSpans(rec), spanRunAgent)
	if len(runs) != 1 || runs[0].Status().Code == codes.Error || attrString(runs[0], attrRunResult) != "not_found" {
		t.Fatalf("run span = %v, want a not_found result without an error status", runs)
	}
}

type failingKnowledge struct{}

func (failingKnowledge) Retrieve(context.Context, string, *knowledge.RetrieveParams) ([]knowledge.ScoredChunk, error) {
	return nil, errors.New("index unreachable for alice@example.com")
}

func (failingKnowledge) ListCollections(context.Context) ([]knowledge.CollectionInfo, error) {
	return nil, nil
}

// A failed retrieval still reaches the model as an error payload, and the
// hooks still see a completed call, but the span and the metric say it
// failed.
func TestFailedRetrievalIsReportedAsFailed(t *testing.T) {
	rec := recordSpans(t)
	sink := telemetrytest.NewSink()
	hooks := &toolEventRecorder{}
	e, err := New(
		WithStore(scopespy.New()),
		WithLLM(&twoStepLLM{calls: []llm.ToolCall{{ID: "call-1", Name: "knowledge_search", Arguments: `{"query":"QUERY-CANARY"}`}}, final: "ok"}),
		WithKnowledge(failingKnowledge{}),
		WithMetrics(sink),
		WithExtension(hooks),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := e.RunAgent(wsCtx(), "assistant", "look it up", nil); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if countEvent(hooks.snapshot(), "completed") != 1 || countEvent(hooks.snapshot(), "failed") != 0 {
		t.Errorf("hook events = %v, want the call still reported completed", hooks.snapshot())
	}
	if got := sink.Value(metricToolCallsHandled, map[string]string{"kind": "builtin", "outcome": "failed"}); got != 1 {
		t.Errorf("builtin failed = %v, want 1", got)
	}
	tools := spansNamed(cortexSpans(rec), spanCallTool)
	if len(tools) != 1 || attrString(tools[0], attrToolOutcome) != "failed" || tools[0].Status().Code != codes.Error {
		t.Fatalf("call tool spans = %v", tools)
	}
	assertNoCanaries(t, rec, sink, []string{"alice@example.com", "QUERY-CANARY", "unreachable"})
}

func TestToolCallIDIsBounded(t *testing.T) {
	for in, ok := range map[string]bool{
		"call_abc123":              true,
		"toolu_01A-b.c:d":          true,
		"":                         false,
		strings.Repeat("a", 65):    false,
		"call alice@example.com":   false,
		"call\nwith\nnewlines":     false,
		strings.Repeat("b", 64):    true,
		"ignore previous; drop it": false,
	} {
		if _, got := toolCallID(in); got != ok {
			t.Errorf("toolCallID(%q) kept = %v, want %v", in, got, ok)
		}
	}
}
