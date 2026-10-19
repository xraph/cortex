package a2a

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/telemetry/telemetrytest"
)

func recordBusSpans(t *testing.T) *tracetest.SpanRecorder {
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

// A delivery is a consumer root linked to the span that sent it, carries
// no envelope content, and is counted by class, route and result. A drain
// with nothing queued starts nothing.
func TestDeliverySpanAndCounter(t *testing.T) {
	rec := recordBusSpans(t)
	sink := telemetrytest.NewSink()
	b, err := NewBus(BusConfig{
		Store: newMemStore(), Runner: newFakeRunner(), Resumer: newFakeResumer(),
		Clock: &fakeClock{now: testNow}, Synchronous: true, Metrics: sink,
		Options: Options{HopCeiling: 3, Workers: 1},
	})
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}

	if _, err := b.Drain(testCtx()); err != nil {
		t.Fatalf("idle Drain: %v", err)
	}
	if n := len(rec.Ended()); n != 0 {
		t.Fatalf("an idle drain started %d span(s)", n)
	}

	sendCtx, sender := otel.Tracer("test").Start(testCtx(), "sender")
	if _, err := b.Send(sendCtx, SendParams{
		Sender:       Address{Agent: "planner"},
		Receivers:    []Address{{Agent: "w1"}},
		Performative: Inform,
		Content:      "the build is green, tell alice@example.com",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	sender.End()
	if _, err := b.Drain(testCtx()); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	var deliveries []sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == spanDeliverMessage {
			deliveries = append(deliveries, s)
		}
	}
	if len(deliveries) != 1 {
		t.Fatalf("%d delivery spans, want 1", len(deliveries))
	}
	d := deliveries[0]
	if d.Parent().IsValid() || d.SpanKind() != trace.SpanKindConsumer {
		t.Errorf("delivery span: parent valid = %v, kind = %v", d.Parent().IsValid(), d.SpanKind())
	}
	if len(d.Links()) != 1 || d.Links()[0].SpanContext.SpanID() != sender.SpanContext().SpanID() {
		t.Errorf("delivery links = %v, want the sender's span", d.Links())
	}
	got := map[string]string{}
	for _, kv := range d.Attributes() {
		got[string(kv.Key)] = kv.Value.String()
		if strings.Contains(kv.Value.String(), "alice@example.com") || strings.Contains(kv.Value.String(), "planner") || strings.Contains(kv.Value.String(), "w1") {
			t.Errorf("delivery attribute %s carries envelope content or an agent name: %q", kv.Key, kv.Value.String())
		}
	}
	for k, want := range map[string]string{attrClass: "informative", attrRoute: "local", attrResult: "delivered", attrPerformative: "inform"} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	if v := sink.Value(metricDeliveriesAttempted, map[string]string{"class": "informative", "route": "local", "result": "delivered"}); v != 1 {
		t.Errorf("deliveries counted = %v, want 1", v)
	}
}

// The producer link table is memory only and bounded: a row another
// process carries is never taken here, and must not grow it forever.
func TestProducerLinksStayBounded(t *testing.T) {
	var p producerLinks
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled})
	start := time.Now()
	for range maxProducerLinks + 10 {
		p.put(id.NewDeliveryID(), sc, start)
	}
	if n := len(p.m); n != maxProducerLinks {
		t.Fatalf("%d links held, want the cap of %d", n, maxProducerLinks)
	}
	late := id.NewDeliveryID()
	p.put(late, sc, start.Add(producerLinkTTL+time.Minute))
	if n := len(p.m); n != 1 {
		t.Fatalf("%d links held after the old ones aged out, want 1", n)
	}
	if got := p.take(late); !got.Equal(sc) {
		t.Error("the fresh link was not kept")
	}
	if got := p.take(late); got.IsValid() {
		t.Error("a link was handed out twice")
	}
}
