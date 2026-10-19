package a2a

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	gometrics "github.com/xraph/go-utils/metrics"

	"github.com/xraph/cortex/id"
)

// The bus is a leaf package (see imports_test.go), so it carries its own
// small copies of the engine's telemetry helpers rather than importing them.
// Same tracer scope, same rules: no error text on a span, bounded labels.

// tracer returns the cortex tracer from the global provider.
func tracer() trace.Tracer { return otel.Tracer("github.com/xraph/cortex") }

// stripSpan keeps ctx's values and cancellation and drops its span.
func stripSpan(ctx context.Context) context.Context {
	return trace.ContextWithSpanContext(ctx, trace.SpanContext{})
}

// failSpan marks span failed with the error's type, never its text: a
// delivery error can quote envelope content or a peer's URL.
func failSpan(span trace.Span, err error) {
	class := errorType(err)
	span.SetStatus(codes.Error, class)
	span.AddEvent("exception", trace.WithAttributes(attribute.String("exception.type", class)))
}

func errorType(err error) string {
	for _, s := range []struct {
		err  error
		name string
	}{
		{context.Canceled, "context.Canceled"},
		{context.DeadlineExceeded, "context.DeadlineExceeded"},
		{ErrDeliveryAlreadyClaimed, "a2a.ErrDeliveryAlreadyClaimed"},
		{ErrConversationClosed, "a2a.ErrConversationClosed"},
		{ErrHopCeiling, "a2a.ErrHopCeiling"},
		{ErrUnroutable, "a2a.ErrUnroutable"},
		{ErrUnknownReceiver, "a2a.ErrUnknownReceiver"},
	} {
		if errors.Is(err, s.err) {
			return s.name
		}
	}
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return fmt.Sprintf("%T", err)
		}
		err = next
	}
}

// counterVec is a labelled counter that asks the sink for each series once.
// A nil *counterVec records nothing.
type counterVec struct {
	m     gometrics.Metrics
	name  string
	help  string
	keys  []string
	cache sync.Map // label values joined by \xff -> gometrics.Counter
}

func newCounterVec(m gometrics.Metrics, name, help string, keys ...string) *counterVec {
	if m == nil {
		return nil
	}
	return &counterVec{m: m, name: name, help: help, keys: keys}
}

func (c *counterVec) inc(values ...string) {
	if c == nil || len(values) != len(c.keys) {
		return
	}
	key := strings.Join(values, "\xff")
	if v, ok := c.cache.Load(key); ok {
		if counter, isCounter := v.(gometrics.Counter); isCounter {
			counter.Inc()
		}
		return
	}
	labels := make(map[string]string, len(c.keys))
	for i, k := range c.keys {
		labels[k] = values[i]
	}
	v, _ := c.cache.LoadOrStore(key, c.m.Counter(c.name, gometrics.WithDescription(c.help), gometrics.WithLabels(labels)))
	if counter, isCounter := v.(gometrics.Counter); isCounter {
		counter.Inc()
	}
}

// links turns span contexts into span links, skipping invalid ones and
// repeats.
func links(scs ...trace.SpanContext) []trace.Link {
	var out []trace.Link
	for _, sc := range scs {
		if !sc.IsValid() {
			continue
		}
		dup := false
		for _, l := range out {
			if l.SpanContext.Equal(sc) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, trace.Link{SpanContext: sc})
		}
	}
	return out
}

// spanDeliverMessage is the root span of one claimed delivery. A directive's
// run agent span nests under it.
const spanDeliverMessage = "deliver agent message"

// metricDeliveriesAttempted counts claimed deliveries by routing class,
// route and result. A Forge app scrapes it as
// forge_cortex_a2a_deliveries_attempted_total.
const metricDeliveriesAttempted = "cortex_a2a_deliveries_attempted_total"

// Span attributes. Envelope content, addresses (which name agents) and peer
// details never go on a span; the IDs are opaque.
const (
	attrDeliveryID     = "cortex.a2a.delivery.id"
	attrMessageID      = "cortex.a2a.message.id"
	attrConversationID = "cortex.a2a.conversation.id"
	attrPerformative   = "cortex.a2a.performative"
	attrClass          = "cortex.a2a.class"
	attrRoute          = "cortex.a2a.route"
	attrResult         = "cortex.a2a.result"
)

// Delivery result values.
const (
	resultDelivered = "delivered"
	resultFailed    = "failed"
	resultError     = "error"
)

type busMetrics struct {
	deliveries *counterVec
}

func newBusMetrics(m gometrics.Metrics) busMetrics {
	return busMetrics{deliveries: newCounterVec(m, metricDeliveriesAttempted,
		"Agent-to-agent deliveries the bus claimed, by routing class, route and result.",
		"class", "route", "result")}
}

// producerLinks remembers which span queued each delivery, so the delivery's
// own trace can link back to it. It is memory only: nothing is added to the
// envelope or the delivery row, and a delivery carried after a restart, or
// by another process, simply has no link. It is bounded, because a row this
// process queued can be carried by another one and never be taken here.
type producerLinks struct {
	mu sync.Mutex
	m  map[id.DeliveryID]producerLink
}

type producerLink struct {
	sc trace.SpanContext
	at time.Time
}

const (
	maxProducerLinks = 4096
	producerLinkTTL  = 10 * time.Minute
)

func (p *producerLinks) put(deliveryID id.DeliveryID, sc trace.SpanContext, now time.Time) {
	if !sc.IsValid() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = make(map[id.DeliveryID]producerLink)
	}
	if len(p.m) >= maxProducerLinks {
		for k, v := range p.m {
			if now.Sub(v.at) > producerLinkTTL {
				delete(p.m, k)
			}
		}
		if len(p.m) >= maxProducerLinks {
			return
		}
	}
	p.m[deliveryID] = producerLink{sc: sc, at: now}
}

func (p *producerLinks) take(deliveryID id.DeliveryID) trace.SpanContext {
	p.mu.Lock()
	defer p.mu.Unlock()
	l, ok := p.m[deliveryID]
	if !ok {
		return trace.SpanContext{}
	}
	delete(p.m, deliveryID)
	return l.sc
}

// deliveryTrace is one claimed delivery's span and the labels it is counted
// under.
type deliveryTrace struct {
	span  trace.Span
	m     busMetrics
	class string
	route string
}

// startDelivery opens the delivery's root span. It links to the span that
// queued the delivery when this process remembers it, and to the span on
// ctx when there is one: the start hook's span for a startup redrive, or a
// caller's span for a synchronous Drain. The workers' context carries none.
func (b *Bus) startDelivery(ctx context.Context, d *Delivery, producer trace.SpanContext) (context.Context, *deliveryTrace) {
	route := "local"
	if !d.Receiver.IsLocal() {
		route = "remote"
	}
	ctx, span := tracer().Start(ctx, spanDeliverMessage,
		trace.WithNewRoot(),
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithLinks(links(producer, trace.SpanContextFromContext(ctx))...),
		trace.WithAttributes(
			attribute.String(attrDeliveryID, d.ID.String()),
			attribute.String(attrMessageID, d.MessageID.String()),
			attribute.String(attrRoute, route),
		))
	return ctx, &deliveryTrace{span: span, m: b.metrics, class: "unknown", route: route}
}

// envelope records what the loaded message says about itself.
func (t *deliveryTrace) envelope(e *Envelope) {
	performative := "unknown"
	if class, ok := e.Performative.Class(); ok {
		performative = string(e.Performative)
		t.class = string(class)
	}
	t.span.SetAttributes(
		attribute.String(attrConversationID, e.ConversationID.String()),
		attribute.String(attrPerformative, performative),
		attribute.String(attrClass, t.class),
	)
}

// end records how the delivery went. A delivery the bus marked failed (an
// unreachable peer, an invalid performative) is an outcome the sender reads,
// not a fault; only a delivery the bus could not record is an error.
func (t *deliveryTrace) end(d *Delivery, err error) {
	result := resultError
	switch {
	case err != nil:
		failSpan(t.span, err)
	case d.State == DeliveryDelivered:
		result = resultDelivered
	case d.State == DeliveryFailed:
		result = resultFailed
	}
	t.span.SetAttributes(attribute.String(attrResult, result), attribute.String(attrClass, t.class))
	t.m.deliveries.inc(t.class, t.route, result)
	t.span.End()
}
