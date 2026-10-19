package engine

import (
	"context"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/internal/telemetry/telemetrytest"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/store/scopespy"
)

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
