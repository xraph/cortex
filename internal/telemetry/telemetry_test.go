package telemetry

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/safety"
)

type codedError struct{}

func (*codedError) Error() string { return "coded: alice@example.com" }

func TestErrorTypeNamesSentinelsAndTypesNeverText(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("resolve agent: %w", cortex.ErrAgentNotFound), "cortex.ErrAgentNotFound"},
		{fmt.Errorf("check: %w", safety.ErrBlocked), "safety.ErrBlocked"},
		{fmt.Errorf("llm: %w", context.DeadlineExceeded), "context.DeadlineExceeded"},
		{fmt.Errorf("outer: %w", &codedError{}), "*telemetry.codedError"},
		{errors.New("provider said alice@example.com"), "*errors.errorString"},
	} {
		if got := ErrorType(tc.err); got != tc.want {
			t.Errorf("ErrorType(%v) = %s, want %s", tc.err, got, tc.want)
		}
	}
}

func TestDetachStripsTheSpanAndKeepsALink(t *testing.T) {
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled})
	type key struct{}
	ctx := context.WithValue(trace.ContextWithSpanContext(context.Background(), sc), key{}, "kept")

	detached := Detach(ctx)
	if trace.SpanContextFromContext(detached).IsValid() {
		t.Fatal("Detach left the span on the context")
	}
	if detached.Value(key{}) != "kept" {
		t.Error("Detach dropped a context value")
	}
	rest, link, ok := TakeLink(detached)
	if !ok || !link.Equal(sc) {
		t.Fatalf("TakeLink = %v, %v; want the original span", link, ok)
	}
	if _, _, again := TakeLink(rest); again {
		t.Error("the link survived being taken, so a nested run would detach too")
	}
	if _, _, none := TakeLink(StripSpan(ctx)); none {
		t.Error("StripSpan alone recorded a link")
	}
}

func TestNilVecsRecordNothing(t *testing.T) {
	if NewCounterVec(nil, "c_total", "", "k") != nil || NewHistogramVec(nil, "h_seconds", "", "s", nil, "k") != nil || NewGaugeVec(nil, "g", "", "k") != nil {
		t.Fatal("a nil sink should give nil vecs")
	}
	var c *CounterVec
	var h *HistogramVec
	var g *GaugeVec
	c.Inc("v")
	h.Observe(1, "v")
	g.Set(1, "v")
}

func TestThrottleCountsWhatItSkips(t *testing.T) {
	var th Throttle
	start := time.Unix(1000, 0)
	if ok, n := th.Allow("create step", start); !ok || n != 0 {
		t.Fatalf("first line: ok = %v, suppressed = %d", ok, n)
	}
	for i := range 3 {
		if ok, _ := th.Allow("create step", start.Add(time.Duration(i+1)*time.Second)); ok {
			t.Fatal("a second line inside the interval was allowed")
		}
	}
	if ok, _ := th.Allow("save conversation", start.Add(time.Second)); !ok {
		t.Error("one site's throttle held back another's")
	}
	ok, n := th.Allow("create step", start.Add(LogInterval))
	if !ok || n != 3 {
		t.Fatalf("after the interval: ok = %v, suppressed = %d, want true and 3", ok, n)
	}
}
