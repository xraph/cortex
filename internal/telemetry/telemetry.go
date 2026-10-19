// Package telemetry holds the tracing and metrics helpers the engine and the
// message bus share. It lives in the root module, so it uses only OpenTelemetry
// and go-utils and never forge: a Forge app installs the global tracer provider
// and hands the engine its metrics, and a host without Forge gets no-op spans
// and no metrics until it does the same.
package telemetry

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/safety"
)

// ScopeName is the instrumentation scope every cortex span carries.
const ScopeName = "github.com/xraph/cortex"

// Tracer returns the cortex tracer from the global provider. It is looked up
// per call rather than cached, so a provider installed after the engine was
// built (Forge installs its own at start) is still the one that records.
func Tracer() trace.Tracer { return otel.Tracer(ScopeName) }

// Fail marks span as failed. The description is a short class, never the
// error's text: provider, driver and tool messages quote prompts, arguments
// and stored values. The exception event carries only the error's type.
func Fail(span trace.Span, err error) {
	class := ErrorType(err)
	span.SetStatus(codes.Error, class)
	span.AddEvent("exception", trace.WithAttributes(attribute.String("exception.type", class)))
}

// ErrPanic stands in for a call that panicked. A deferred end records it,
// as the class "panic", while the panic carries on unwinding.
var ErrPanic = errors.New("cortex: panic")

// sentinels names the errors callers branch on. A wrapped sentinel is
// reported by its name, because its Go type (*errors.errorString) says
// nothing.
var sentinels = []struct {
	err  error
	name string
}{
	{ErrPanic, "panic"},
	{context.Canceled, "context.Canceled"},
	{context.DeadlineExceeded, "context.DeadlineExceeded"},
	{safety.ErrBlocked, "safety.ErrBlocked"},
	{safety.ErrUnavailable, "safety.ErrUnavailable"},
	{safety.ErrLimit, "safety.ErrLimit"},
	{cortex.ErrNoStore, "cortex.ErrNoStore"},
	{cortex.ErrNoLLM, "cortex.ErrNoLLM"},
	{cortex.ErrNoScope, "cortex.ErrNoScope"},
	{cortex.ErrAgentNotFound, "cortex.ErrAgentNotFound"},
	{cortex.ErrRunNotFound, "cortex.ErrRunNotFound"},
	{cortex.ErrOrchestrationNotFound, "cortex.ErrOrchestrationNotFound"},
	{cortex.ErrInvalidState, "cortex.ErrInvalidState"},
	{cortex.ErrInvalidPrepared, "cortex.ErrInvalidPrepared"},
	{cortex.ErrPreparationUnavailable, "cortex.ErrPreparationUnavailable"},
	{cortex.ErrPreparationConflict, "cortex.ErrPreparationConflict"},
	{cortex.ErrRunCancelled, "cortex.ErrRunCancelled"},
	{cortex.ErrRunIncomplete, "cortex.ErrRunIncomplete"},
	{cortex.ErrMaxStepsReached, "cortex.ErrMaxStepsReached"},
	{cortex.ErrRequiresApproval, "cortex.ErrRequiresApproval"},
	{cortex.ErrNotSuspended, "cortex.ErrNotSuspended"},
	{cortex.ErrSuspensionExpired, "cortex.ErrSuspensionExpired"},
	{cortex.ErrResultsMismatch, "cortex.ErrResultsMismatch"},
	{cortex.ErrInvalidContinuation, "cortex.ErrInvalidContinuation"},
}

// ErrorType names err for a span: a known sentinel by its name, anything else
// by the Go type of the innermost error in its wrap chain.
func ErrorType(err error) string {
	if err == nil {
		return ""
	}
	for _, s := range sentinels {
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

// StripSpan keeps ctx's values and cancellation but drops its span, so work
// started from it begins a trace of its own. A loop started from a Forge
// start hook or a request needs this: context.WithoutCancel and WithCancel
// keep the span, and every query an idle tick made would land in a trace
// that ended long ago.
func StripSpan(ctx context.Context) context.Context {
	return trace.ContextWithSpanContext(ctx, trace.SpanContext{})
}

type linkKey struct{}

// Detach strips ctx's span and remembers it as a link target. The next run
// span started from the result is a new root that links back to the span ctx
// carried, instead of a child that outlives its parent.
func Detach(ctx context.Context) context.Context {
	sc := trace.SpanContextFromContext(ctx)
	ctx = StripSpan(ctx)
	if sc.IsValid() {
		ctx = context.WithValue(ctx, linkKey{}, sc)
	}
	return ctx
}

// TakeLink returns the link target Detach recorded, and a ctx that no longer
// carries it, so a span nested under the new root is an ordinary child.
func TakeLink(ctx context.Context) (context.Context, trace.SpanContext, bool) {
	sc, ok := ctx.Value(linkKey{}).(trace.SpanContext)
	if !ok || !sc.IsValid() {
		return ctx, trace.SpanContext{}, false
	}
	return context.WithValue(ctx, linkKey{}, trace.SpanContext{}), sc, true
}

// Links turns span contexts into span links, skipping invalid ones and
// repeats.
func Links(scs ...trace.SpanContext) []trace.Link {
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
