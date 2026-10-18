package engine

import (
	"context"
	"fmt"
	"time"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

// finalizeRun persists termination before notifying completion observers. Repeated
// finalization of the same in-memory run is harmless. Distributed arbitration
// requires a store claim; this helper does not promise exactly-once delivery.
func (e *Engine) finalizeRun(ctx context.Context, r *run.Run, outcome run.Outcome) error {
	if isTerminalRunState(r.State) {
		return nil
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	saved, err := e.store.GetRun(writeCtx, r.ID)
	if err != nil {
		e.logger.Warn("read before terminal update; using owned run snapshot", log.String("error", err.Error()))
	}
	if err == nil && isTerminalRunState(saved.State) {
		*r = *saved
		return nil
	}
	next := *r
	next.State = run.StateIncomplete
	switch outcome.StopReason {
	case run.StopFinalAnswer:
		next.State = run.StateCompleted
	case run.StopCancelled:
		next.State = run.StateCancelled
	}
	if ctx.Err() != nil {
		next.State = run.StateCancelled
		outcome.StopReason = run.StopCancelled
		if ctx.Err() == context.DeadlineExceeded {
			next.State = run.StateIncomplete
			outcome.StopReason = run.StopDeadline
		}
	}
	next.StopReason = outcome.StopReason
	next.FinishReason = outcome.FinishReason
	next.VerificationStatus = verification.StatusUnchecked
	next.Output = outcome.Output
	next.StepCount = outcome.StepCount
	next.TokensUsed = outcome.TokensUsed
	if outcome.CompletedAt.IsZero() {
		outcome.CompletedAt = time.Now().UTC()
	}
	next.CompletedAt = &outcome.CompletedAt
	if err := e.store.UpdateRun(writeCtx, &next); err != nil {
		return fmt.Errorf("cortex: persist terminal outcome: %w", err)
	}
	*r = next
	if r.State == run.StateCompleted {
		var duration time.Duration
		if r.StartedAt != nil {
			duration = outcome.CompletedAt.Sub(*r.StartedAt)
		}
		e.extensions.EmitRunCompleted(writeCtx, r.AgentID, r.ID, r.Output, duration)
	}
	return nil
}

func responseStopReason(finish, content string, toolCount int) run.StopReason {
	if toolCount == 0 {
		switch finish {
		case "tool_calls", "function_call", "tool_use":
			return run.StopInvalidResponse
		case "":
			if content == "" {
				return run.StopInvalidResponse
			}
		}
	}
	switch finish {
	case "", "stop", "end_turn", "tool_calls", "function_call", "tool_use":
		return run.StopFinalAnswer
	case "length", "max_tokens":
		return run.StopTokenLimit
	case "invalid_response":
		return run.StopInvalidResponse
	case "content_filter", "refusal":
		return run.StopSafetyBlock
	default:
		return run.StopProviderUnknown
	}
}

// outcomeEvent is emitted only after the durable terminal update succeeds.
func outcomeEvent(r *run.Run, duration time.Duration) StreamEvent {
	kind := EventIncomplete
	if r.State == run.StateCompleted {
		kind = EventDone
	}
	return StreamEvent{Type: kind, Data: map[string]any{
		"run_id": r.ID.String(), "output": r.Output, "tokens_used": r.TokensUsed,
		"state": string(r.State), "stop_reason": string(r.StopReason),
		"verification_status": string(r.VerificationStatus.Normalized()), "duration_ms": duration.Milliseconds(),
	}}
}
