package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/telemetry/telemetrytest"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/store/scopespy"
	sqlitestore "github.com/xraph/cortex/store/sqlite"
)

// recordSpans installs a recording tracer provider as the global one for
// the test and puts the previous provider back afterwards.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})
	return rec
}

// cortexSpans keeps the spans this module started, by instrumentation scope.
func cortexSpans(rec *tracetest.SpanRecorder) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.InstrumentationScope().Name == "github.com/xraph/cortex" {
			out = append(out, s)
		}
	}
	return out
}

func spansNamed(spans []sdktrace.ReadOnlySpan, name string) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

func attrOf(s sdktrace.ReadOnlySpan, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func attrString(s sdktrace.ReadOnlySpan, key string) string {
	v, _ := attrOf(s, key)
	return v.String()
}

func wsCtx() context.Context {
	return cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
}

// approvingGate approves everything and remembers what it was shown, so a
// test can prove a value went through publication and still never reached
// a span.
type approvingGate struct {
	mu   sync.Mutex
	seen []string
}

func (g *approvingGate) Check(_ context.Context, r safety.PublicationRequest) (safety.PublicationDecision, error) {
	g.mu.Lock()
	g.seen = append(g.seen, r.Content)
	g.mu.Unlock()
	return safety.PublicationDecision{Text: r.Content, Status: safety.StatusApproved}, nil
}

func (g *approvingGate) saw(s string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range g.seen {
		if strings.Contains(c, s) {
			return true
		}
	}
	return false
}

// twoStepLLM asks for the given tool calls once, then answers.
type twoStepLLM struct {
	mu     sync.Mutex
	calls  []llm.ToolCall
	first  string
	final  string
	called bool
}

func (m *twoStepLLM) Complete(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.called {
		m.called = true
		return &llm.Response{Content: m.first, ToolCalls: m.calls, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 7}}, nil
	}
	return &llm.Response{Content: m.final, FinishReason: "stop", Usage: llm.Usage{TotalTokens: 5}}, nil
}

func (m *twoStepLLM) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, errors.New("twoStepLLM: no streaming")
}

// The values a user, a model or a tool wrote. None of them may reach a
// span, an event, a status or the metric scrape.
var canaries = []string{
	"alice@example.com",
	"PROMPT-CANARY",
	"INPUT-CANARY",
	"ARG-CANARY",
	"RESULT-CANARY",
	"OUTPUT-CANARY",
	"steal_",
	"secret-agent-name",
}

// spanText flattens everything a span carries that an exporter would ship.
func spanText(s sdktrace.ReadOnlySpan) string {
	var b strings.Builder
	b.WriteString(s.Name())
	b.WriteByte('\n')
	for _, kv := range s.Attributes() {
		b.WriteString(string(kv.Key) + "=" + kv.Value.String() + "\n")
	}
	for _, ev := range s.Events() {
		b.WriteString(ev.Name + "\n")
		for _, kv := range ev.Attributes {
			b.WriteString(string(kv.Key) + "=" + kv.Value.String() + "\n")
		}
	}
	for _, l := range s.Links() {
		for _, kv := range l.Attributes {
			b.WriteString(string(kv.Key) + "=" + kv.Value.String() + "\n")
		}
	}
	b.WriteString(s.Status().Description)
	return b.String()
}

// TestRunTelemetryCarriesNoUserContent drives one run through the whole
// loop, a registered tool call, a call to a tool the model made up, and
// every publication check in between, with recognisable values in the
// system prompt, the input, the tool arguments, the tool result, the model
// output and the agent name. The spans and the metrics must describe the
// run completely and quote none of it.
func TestRunTelemetryCarriesNoUserContent(t *testing.T) {
	rec := recordSpans(t)
	sink := telemetrytest.NewSink()
	gate := &approvingGate{}
	model := &twoStepLLM{
		first: "looking up OUTPUT-CANARY",
		calls: []llm.ToolCall{
			{ID: "call-1", Name: "lookup_customer", Arguments: `{"email":"alice@example.com","note":"ARG-CANARY"}`},
			{ID: "call-2", Name: "steal_alice@example.com", Arguments: `{"x":"ARG-CANARY"}`},
			{ID: "call-3", Name: "flaky_tool", Arguments: `{"who":"alice@example.com"}`},
		},
		final: "final OUTPUT-CANARY answer for alice@example.com",
	}
	e, err := New(
		WithStore(scopespy.New()),
		WithLLM(model),
		WithMetrics(sink),
		WithPublicationGate(gate),
		WithTool(llm.Tool{Name: "lookup_customer", Description: "test-only"}, func(context.Context, cortex.Invocation) (string, error) {
			return `{"customer":"alice@example.com","flag":"RESULT-CANARY"}`, nil
		}),
		WithTool(llm.Tool{Name: "flaky_tool", Description: "test-only"}, func(context.Context, cortex.Invocation) (string, error) {
			return "", errors.New("upstream refused alice@example.com RESULT-CANARY")
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	r, err := e.RunAgent(wsCtx(), "secret-agent-name", "INPUT-CANARY my email is alice@example.com",
		&RunOverrides{SystemPrompt: "PROMPT-CANARY you serve alice@example.com"})
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if r.State != run.StateCompleted {
		t.Fatalf("State = %s, want completed", r.State)
	}
	for _, want := range []string{"INPUT-CANARY", "ARG-CANARY", "RESULT-CANARY", "OUTPUT-CANARY"} {
		if !gate.saw(want) {
			t.Fatalf("the publication gate never saw %q; the test is not exercising the checks it claims to", want)
		}
	}

	spans := cortexSpans(rec)
	for _, s := range rec.Ended() {
		text := spanText(s)
		for _, c := range canaries {
			if strings.Contains(text, c) {
				t.Errorf("span %q (scope %s) carries %q:\n%s", s.Name(), s.InstrumentationScope().Name, c, text)
			}
		}
	}
	scrape := sink.Scrape()
	for _, c := range canaries {
		if strings.Contains(scrape, c) {
			t.Errorf("the metric scrape carries %q:\n%s", c, scrape)
		}
	}

	// The same spans describe the run fully.
	runs := spansNamed(spans, spanRunAgent)
	if len(runs) != 1 {
		t.Fatalf("%d run agent spans, want 1", len(runs))
	}
	rs := runs[0]
	if rs.Parent().IsValid() {
		t.Error("run agent span has a parent; it was started from a bare context")
	}
	for key, want := range map[string]string{
		attrGenAIOperation: "invoke_agent",
		attrRunID:          r.ID.String(),
		attrGenAIAgentID:   r.AgentID.String(),
		attrRunMode:        modeSync,
		attrRunResumed:     "false",
		attrRunState:       "completed",
		attrRunStopReason:  "final_answer",
		attrRunSteps:       "2",
		attrRunTokens:      "12",
		attrRunPurpose:     "normal",
	} {
		if got := attrString(rs, key); got != want {
			t.Errorf("run agent %s = %q, want %q", key, got, want)
		}
	}
	if rs.Status().Code == codes.Error {
		t.Errorf("a completed run is marked failed: %q", rs.Status().Description)
	}

	prep := spansNamed(spans, spanPrepareAgent)
	if len(prep) != 1 || prep[0].Parent().SpanID() != rs.SpanContext().SpanID() {
		t.Fatalf("want one prepare agent span under the run, got %d", len(prep))
	}

	steps := spansNamed(spans, spanRunStep)
	if len(steps) != 2 {
		t.Fatalf("%d step spans, want 2", len(steps))
	}
	outcomes := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range steps {
		if s.Parent().SpanID() != rs.SpanContext().SpanID() {
			t.Errorf("step %s is not a child of the run", attrString(s, attrStepIndex))
		}
		outcomes[attrString(s, attrStepOutcome)] = s
	}
	toolStep, ok := outcomes[stepToolCalls]
	if !ok || outcomes[stepFinal] == nil {
		t.Fatalf("step outcomes = %v, want tool_calls and final", outcomes)
	}
	if got := attrString(toolStep, attrStepToolCalls); got != "3" {
		t.Errorf("tool step tool_calls = %s, want 3", got)
	}

	tools := spansNamed(spans, spanCallTool)
	if len(tools) != 3 {
		t.Fatalf("%d call tool spans, want 3", len(tools))
	}
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range tools {
		if s.Parent().SpanID() != toolStep.SpanContext().SpanID() {
			t.Errorf("call tool %s is not a child of its step", attrString(s, attrGenAIToolName))
		}
		byName[attrString(s, attrGenAIToolName)] = s
	}
	registered := byName["lookup_customer"]
	if registered == nil || attrString(registered, attrToolKind) != toolKindRegistered ||
		attrString(registered, attrToolOutcome) != "completed" || attrString(registered, attrGenAIToolCallID) != "call-1" {
		t.Errorf("registered tool span = %v", registered)
	}
	// The prepared allowlist refuses a name the model made up before
	// dispatch, so it is a denial, which is not an error.
	invented := byName[labelUnknown]
	if invented == nil || attrString(invented, attrToolKind) != toolKindUnknown || attrString(invented, attrToolOutcome) != "denied" {
		t.Fatalf("a model-invented tool name should be reported as unknown and denied, got %v", byName)
	}
	if invented.Status().Code == codes.Error {
		t.Errorf("a denied call is marked as an error: %v", invented.Status())
	}
	flaky := byName["flaky_tool"]
	if flaky == nil || attrString(flaky, attrToolOutcome) != "failed" {
		t.Fatalf("flaky tool span = %v, want a failed call", flaky)
	}
	if flaky.Status().Code != codes.Error || flaky.Status().Description != "*errors.errorString" {
		t.Errorf("failed tool status = %v, want an error named by its type", flaky.Status())
	}

	checks := spansNamed(spans, spanCheckPublication)
	if len(checks) == 0 {
		t.Fatal("no check publication spans, though a gate was consulted")
	}
	sources := map[string]bool{}
	for _, s := range checks {
		sources[attrString(s, attrPublicationSource)] = true
	}
	for _, want := range []string{"user_input", "model_output", "tool_call", "tool_result", "provider_finish"} {
		if !sources[want] {
			t.Errorf("no check publication span for source %s (got %v)", want, sources)
		}
	}

	for _, c := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{metricRunsFinished, map[string]string{"state": "completed", "stop_reason": "final_answer", "purpose": "normal"}, 1},
		{metricRunDuration, map[string]string{"state": "completed", "purpose": "normal"}, 1},
		{metricRunsInProgress, map[string]string{"mode": "sync"}, 0},
		{metricStepsExecuted, map[string]string{"outcome": "tool_calls"}, 1},
		{metricStepsExecuted, map[string]string{"outcome": "final"}, 1},
		{metricToolCallsHandled, map[string]string{"kind": "registered", "outcome": "completed"}, 1},
		{metricToolCallsHandled, map[string]string{"kind": "unknown", "outcome": "denied"}, 1},
		{metricToolCallsHandled, map[string]string{"kind": "registered", "outcome": "failed"}, 1},
		{metricToolCallDuration, map[string]string{"kind": "registered", "outcome": "completed"}, 1},
		{metricPublicationChecks, map[string]string{"source": "user_input", "direction": "input", "result": "approved"}, 1},
	} {
		if got := sink.Value(c.name, c.labels); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
	for _, name := range sink.Names() {
		keys := sink.LabelKeys(name)
		for _, k := range keys[1:] {
			if strings.Join(k, ",") != strings.Join(keys[0], ",") {
				t.Errorf("%s series carry different label keys: %v", name, keys)
			}
		}
	}
}

// A run started under a caller's span is its child; a detached run is a
// new root that links back to it.
func TestRunSpanParentsAndDetach(t *testing.T) {
	rec := recordSpans(t)
	e, err := New(WithStore(scopespy.New()), WithLLM(&llm.MockClient{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, parent := otel.Tracer("test").Start(wsCtx(), "caller")
	if _, err := e.RunAgent(ctx, "assistant", "hello", nil); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	events := make(chan StreamEvent, 64)
	if err := e.StreamAgent(DetachTrace(ctx), "assistant", "hello", nil, events); err != nil {
		t.Fatalf("StreamAgent: %v", err)
	}
	received := 0
	for range events {
		received++
	}
	if received == 0 {
		t.Fatal("the stream sent no events")
	}
	parent.End()

	runs := spansNamed(cortexSpans(rec), spanRunAgent)
	if len(runs) != 2 {
		t.Fatalf("%d run agent spans, want 2", len(runs))
	}
	var child, detached sdktrace.ReadOnlySpan
	for _, s := range runs {
		if attrString(s, attrRunMode) == modeStream {
			detached = s
		} else {
			child = s
		}
	}
	if child.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Error("a synchronous run is not a child of the caller's span")
	}
	if detached.Parent().IsValid() {
		t.Error("a detached run still has a parent")
	}
	if len(detached.Links()) != 1 || detached.Links()[0].SpanContext.SpanID() != parent.SpanContext().SpanID() {
		t.Errorf("a detached run should link to the span it left, links = %v", detached.Links())
	}
	if attrString(detached, attrRunState) != "completed" {
		t.Errorf("the stream's span ended before its run did: state = %q", attrString(detached, attrRunState))
	}
	for _, s := range spansNamed(cortexSpans(rec), spanRunStep) {
		if s.Parent().TraceID() == parent.SpanContext().TraceID() && s.Parent().SpanID() != child.SpanContext().SpanID() {
			t.Error("a detached run's step landed in the caller's trace")
		}
	}
}

// A run that pauses is not a failure, and the resume is a second pass of
// its own, marked resumed.
func TestSuspendAndResumeTelemetry(t *testing.T) {
	rec := recordSpans(t)
	sink := telemetrytest.NewSink()
	def := externalTool()
	e, err := New(
		WithStore(scopespy.New()),
		WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{{ID: "call-1", Name: def.Name, Arguments: `{"question":"proceed?"}`}}}),
		WithExternalTool(def),
		WithMetrics(sink),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := wsCtx()
	r, err := e.RunAgent(ctx, "assistant", "ask the human", nil)
	if err != nil || r.State != run.StatePaused {
		t.Fatalf("RunAgent = %v, %v; want a paused run", r, err)
	}

	spans := cortexSpans(rec)
	first := spansNamed(spans, spanRunAgent)
	if len(first) != 1 || attrString(first[0], attrRunState) != "paused" || first[0].Status().Code == codes.Error {
		t.Fatalf("paused run span = %v", first)
	}
	if _, has := attrOf(first[0], attrRunStopReason); has {
		t.Error("a paused run has no stop reason yet, but the span carries one")
	}
	suspends := spansNamed(spans, spanSuspendRun)
	if len(suspends) != 1 || attrString(suspends[0], attrSuspensionReason) != "external_tool" ||
		attrString(suspends[0], attrSuspensionPending) != "1" {
		t.Fatalf("suspend run spans = %v", suspends)
	}
	steps := spansNamed(spans, spanRunStep)
	if len(steps) != 1 || attrString(steps[0], attrStepOutcome) != stepSuspended {
		t.Fatalf("step spans = %v, want one suspended step", steps)
	}
	if suspends[0].Parent().SpanID() != steps[0].SpanContext().SpanID() {
		t.Error("suspend run is not a child of the step that paused")
	}
	tools := spansNamed(spans, spanCallTool)
	if len(tools) != 1 || attrString(tools[0], attrToolKind) != toolKindExternal || attrString(tools[0], attrToolOutcome) != "pending" {
		t.Fatalf("call tool spans = %v, want one pending external call", tools)
	}

	if _, err := e.Resume(ctx, r.ID, ResumeInput{ToolResults: []ToolResult{{ToolCallID: "call-1", Content: "yes"}}}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	var resumed sdktrace.ReadOnlySpan
	for _, s := range spansNamed(cortexSpans(rec), spanRunAgent) {
		if attrString(s, attrRunResumed) == "true" {
			resumed = s
		}
	}
	if resumed == nil {
		t.Fatal("no resumed run agent span")
	}
	for key, want := range map[string]string{
		attrRunID:           r.ID.String(),
		attrRunResumeSource: "public",
		attrRunState:        "completed",
		attrRunStopReason:   "final_answer",
	} {
		if got := attrString(resumed, key); got != want {
			t.Errorf("resumed run %s = %q, want %q", key, got, want)
		}
	}

	for _, c := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{metricRunsSuspended, map[string]string{"reason": "external_tool", "result": "ok"}, 1},
		{metricRunDuration, map[string]string{"state": "paused", "purpose": "normal"}, 1},
		{metricRunDuration, map[string]string{"state": "completed", "purpose": "normal"}, 1},
		{metricRunsFinished, map[string]string{"state": "completed", "stop_reason": "final_answer", "purpose": "normal"}, 1},
		{metricToolCallsHandled, map[string]string{"kind": "external", "outcome": "pending"}, 1},
		{metricToolCallDuration, map[string]string{"kind": "external", "outcome": "pending"}, 0},
	} {
		if got := sink.Value(c.name, c.labels); got != c.want {
			t.Errorf("%s%v = %v, want %v", c.name, c.labels, got, c.want)
		}
	}
}

// Every expired row is a root span of its own, even when the caller's
// context carries a span, and the sweeper's idle ticks start none at all.
func TestExpirySweepTelemetry(t *testing.T) {
	rec := recordSpans(t)
	sink := telemetrytest.NewSink()
	s := newSweepStore()
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}}
	r := sweepFixture(t, s, scope, past())
	e, err := New(WithStore(s), WithMetrics(sink))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, parent := otel.Tracer("test").Start(context.Background(), "caller")
	if expireErr := e.expireSuspension(ctx, s.Suspensions()[0], time.Now().UTC()); expireErr != nil {
		t.Fatalf("expireSuspension: %v", expireErr)
	}
	parent.End()

	expiries := spansNamed(cortexSpans(rec), spanExpireSuspension)
	if len(expiries) != 1 {
		t.Fatalf("%d expire suspension spans, want 1", len(expiries))
	}
	ex := expiries[0]
	if ex.Parent().IsValid() {
		t.Error("expire suspension hangs off the caller's span; it must be a root")
	}
	if attrString(ex, attrSuspensionOutcome) != expiryExpired || attrString(ex, attrRunID) != r.ID.String() {
		t.Errorf("expire suspension attributes = %v", ex.Attributes())
	}
	if got := sink.Value(metricSuspensionsExpired, map[string]string{"reason": "external_tool", "result": "ok"}); got != 1 {
		t.Errorf("expired counter = %v, want 1", got)
	}
	if got := sink.Value(metricRunsFinished, map[string]string{"state": "failed", "stop_reason": "execution_error", "purpose": "normal"}); got != 1 {
		t.Errorf("runs finished as failed = %v, want 1", got)
	}

	// Idle ticks: nothing expired, so no span and no query under one.
	idle := newSweepStore()
	var valid atomic.Bool
	ticks := make(chan struct{}, 8)
	idle.onList = func(ctx context.Context) {
		if trace.SpanContextFromContext(ctx).IsValid() {
			valid.Store(true)
		}
		select {
		case ticks <- struct{}{}:
		default:
		}
	}
	idleEngine, err := New(WithStore(idle), WithSuspensionSweepInterval(5*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	before := len(rec.Ended())
	startCtx, boot := otel.Tracer("test").Start(context.Background(), "start cortex")
	if err := idleEngine.Start(startCtx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	boot.End()
	for range 2 {
		select {
		case <-ticks:
		case <-time.After(5 * time.Second):
			t.Fatal("the sweeper never ticked")
		}
	}
	if err := idleEngine.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if valid.Load() {
		t.Error("an idle sweep queried under a live span")
	}
	for _, sp := range rec.Ended()[before:] {
		if sp.InstrumentationScope().Name == "github.com/xraph/cortex" {
			t.Errorf("an idle sweep started span %q", sp.Name())
		}
	}
}

// A run of history checks is one span, however long the history is, and
// every check in it is still counted.
func TestHistoryChecksShareOneSpan(t *testing.T) {
	rec := recordSpans(t)
	sink := telemetrytest.NewSink()
	e, err := New(WithStore(scopespy.New()), WithMetrics(sink), WithPublicationGate(&approvingGate{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	msgs := []llm.Message{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}, {Role: "user", Content: "c"}}
	if err := e.checkMessages(wsCtx(), &agent.Config{ID: id.NewAgentID()}, nil, e.frozenPublication(), msgs); err != nil {
		t.Fatalf("checkMessages: %v", err)
	}
	checks := spansNamed(cortexSpans(rec), spanCheckPublication)
	if len(checks) != 1 || attrString(checks[0], attrPublicationSource) != "history" || attrString(checks[0], attrPublicationChecks) != "3" {
		t.Fatalf("check publication spans = %v, want one history batch of 3", checks)
	}
	if got := sink.Value(metricPublicationChecks, map[string]string{"source": "history", "direction": "input", "result": "approved"}); got != 3 {
		t.Errorf("history checks counted = %v, want 3", got)
	}
}

// idleSpanStore records whether the bus's background queries ran under a
// live span once Start has returned.
type idleSpanStore struct {
	*sqlitestore.Store

	started atomic.Bool
	valid   atomic.Bool
	drains  chan struct{}
	sweeps  chan struct{}
}

func (s *idleSpanStore) note(ctx context.Context, ch chan struct{}) {
	if !s.started.Load() {
		return
	}
	if trace.SpanContextFromContext(ctx).IsValid() {
		s.valid.Store(true)
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *idleSpanStore) ListQueuedDeliveries(ctx context.Context, limit int) ([]*a2a.Delivery, error) {
	s.note(ctx, s.drains)
	return s.Store.ListQueuedDeliveries(ctx, limit)
}

func (s *idleSpanStore) ListExpiredAsks(ctx context.Context, now time.Time, limit int) ([]*a2a.PendingAsk, error) {
	s.note(ctx, s.sweeps)
	return s.Store.ListExpiredAsks(ctx, now, limit)
}

// The delivery workers and the ask sweep outlive Start, whose context
// carries the start hook's span in a Forge app. Their idle queries must
// not land in that trace, and an idle drain starts no span.
func TestBusLoopsDropTheStartSpan(t *testing.T) {
	rec := recordSpans(t)
	ctx := wsCtx()
	st := &idleSpanStore{Store: newApprovalStore(ctx, t), drains: make(chan struct{}, 8), sweeps: make(chan struct{}, 8)}
	e, err := New(
		WithStore(st),
		WithLLM(&llm.MockClient{}),
		WithSuspensionTTL(0),
		WithA2A(a2a.Options{Workers: 1, SweepInterval: 5 * time.Millisecond}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	startCtx, boot := otel.Tracer("test").Start(ctx, "start cortex")
	if err := e.Start(startCtx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st.started.Store(true)
	boot.End()
	for _, ch := range []chan struct{}{st.drains, st.drains, st.sweeps, st.sweeps} {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("a bus loop never ticked")
		}
	}
	if err := e.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st.valid.Load() {
		t.Error("a background bus query ran under the start hook's span")
	}
	for _, s := range cortexSpans(rec) {
		t.Errorf("an idle bus started span %q", s.Name())
	}
}

// An ask crosses the queue: the delivery is a root span that links to the
// pause that queued it and to the caller that drained it, and both the
// worker's run and the asker's resume nest under the delivery.
func TestAskDeliveryLinksAcrossTheQueue(t *testing.T) {
	rec := recordSpans(t)
	sink := telemetrytest.NewSink()
	ctx := wsCtx()
	st := newApprovalStore(ctx, t)
	e, err := New(WithStore(st), WithLLM(&routingLLM{}), WithMetrics(sink), WithA2A(a2a.Options{HopCeiling: 6, Workers: 1}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, name := range []string{"planner", "worker"} {
		if createErr := e.CreateAgent(ctx, &agent.Config{ID: id.NewAgentID(), Name: name, SystemPrompt: "you are " + name, Model: "test-model", MaxSteps: 4}); createErr != nil {
			t.Fatalf("CreateAgent: %v", createErr)
		}
	}
	runCtx, caller := otel.Tracer("test").Start(ctx, "caller")
	paused, err := e.RunAgent(runCtx, "planner", "find out how the worker is doing", nil)
	caller.End()
	if err != nil || paused.State != run.StatePaused {
		t.Fatalf("RunAgent = %v, %v; want paused", paused, err)
	}
	drainCtx, drainer := otel.Tracer("test").Start(ctx, "drainer")
	if _, err := e.A2A().Drain(drainCtx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	drainer.End()

	spans := cortexSpans(rec)
	suspends := spansNamed(spans, spanSuspendRun)
	if len(suspends) != 1 || attrString(suspends[0], attrSuspensionReason) != "agent_reply" {
		t.Fatalf("suspend run spans = %v", suspends)
	}
	var delivery sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == "deliver agent message" {
			if delivery != nil {
				t.Fatal("more than one delivery span for one ask")
			}
			delivery = s
		}
	}
	if delivery == nil {
		t.Fatal("no deliver agent message span")
	}
	if delivery.Parent().IsValid() || delivery.SpanKind() != trace.SpanKindConsumer {
		t.Errorf("delivery span: parent valid = %v, kind = %v; want a consumer root", delivery.Parent().IsValid(), delivery.SpanKind())
	}
	linked := map[trace.SpanID]bool{}
	for _, l := range delivery.Links() {
		linked[l.SpanContext.SpanID()] = true
	}
	if !linked[suspends[0].SpanContext().SpanID()] || !linked[drainer.SpanContext().SpanID()] {
		t.Errorf("delivery links = %v, want the pause that queued it and the drain that carried it", delivery.Links())
	}
	for key, want := range map[string]string{
		"cortex.a2a.class": "directive", "cortex.a2a.route": "local", "cortex.a2a.result": "delivered", "cortex.a2a.performative": "request",
	} {
		if got := attrString(delivery, key); got != want {
			t.Errorf("delivery %s = %q, want %q", key, got, want)
		}
	}

	var worker, resumed sdktrace.ReadOnlySpan
	for _, s := range spansNamed(spans, spanRunAgent) {
		switch {
		case attrString(s, attrRunResumeSource) == "agent_reply":
			resumed = s
		case attrString(s, attrRunID) != paused.ID.String():
			worker = s
		}
	}
	if worker == nil || worker.Parent().SpanID() != delivery.SpanContext().SpanID() {
		t.Error("the worker's run is not a child of the delivery")
	}
	if resumed == nil || resumed.Parent().SpanID() != delivery.SpanContext().SpanID() || attrString(resumed, attrRunState) != "completed" {
		t.Error("the asker's resume is not a completed child of the delivery")
	}
	if got := sink.Value("cortex_a2a_deliveries_attempted_total", map[string]string{"class": "directive", "route": "local", "result": "delivered"}); got != 1 {
		t.Errorf("deliveries counted = %v, want 1", got)
	}
}
