package engine

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

func privateToolContext(ctx context.Context) bool {
	binding, ok := ctx.Value(publicationContextKey{}).(publicationContext)
	return ok && binding.run.Provenance.VerificationRequired
}

func publicToolName(ctx context.Context, name string) string {
	binding, ok := ctx.Value(publicationContextKey{}).(publicationContext)
	if !ok || !binding.run.Provenance.VerificationRequired {
		return name
	}
	for _, tool := range binding.run.Provenance.Tools {
		if tool.Name == name {
			return name
		}
	}
	return "unavailable"
}
func publicToolErrorText(ctx context.Context, text string) string {
	if text != "" && privateToolContext(ctx) {
		return "cortex: tool execution failed"
	}
	return text
}

var errPrivateTool = errors.New("cortex: tool execution failed")

func (e *Engine) emitToolCalled(ctx context.Context, runID id.AgentRunID, name string, args any) {
	if privateToolContext(ctx) {
		args = nil
	}
	e.extensions.EmitToolCalled(ctx, runID, publicToolName(ctx, name), args)
}
func (e *Engine) emitToolCompleted(ctx context.Context, runID id.AgentRunID, name, result string, elapsed time.Duration) {
	if privateToolContext(ctx) {
		result = ""
	}
	e.extensions.EmitToolCompleted(ctx, runID, publicToolName(ctx, name), result, elapsed)
}
func (e *Engine) emitToolFailed(ctx context.Context, runID id.AgentRunID, name string, err error) {
	if privateToolContext(ctx) {
		err = errPrivateTool
	}
	e.extensions.EmitToolFailed(ctx, runID, publicToolName(ctx, name), err)
}
func (e *Engine) emitToolDenied(ctx context.Context, runID id.AgentRunID, name, reason string) {
	e.extensions.EmitToolDenied(ctx, runID, publicToolName(ctx, name), publicToolErrorText(ctx, reason))
}

func closedRequiredError(err error) error {
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded, cortex.ErrResultsMismatch, cortex.ErrInvalidContinuation, cortex.ErrInvalidPrepared, cortex.ErrMaxStepsReached, cortex.ErrRunUnverified} {
		if errors.Is(err, sentinel) {
			return sentinel
		}
	}
	return cortex.ErrVerificationUnavailable
}

func publicToolFailure(ctx context.Context, outcome toolOutcome) string {
	if privateToolContext(ctx) {
		switch outcome {
		case outcomeFailed:
			return errPrivateTool.Error()
		case outcomeDenied:
			return "cortex: tool denied"
		}
	}
	return ""
}
