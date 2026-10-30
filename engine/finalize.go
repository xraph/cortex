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
		return requiredCompletionError(r)
	}
	// A leased run writes its outcome only while the lease is still its
	// own; an interrupted one is handed off instead.
	if leave, err := e.settleLease(ctx, r); leave {
		return err
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	saved, err := e.store.GetRun(writeCtx, r.ID)
	if err != nil {
		e.logWarn("read before terminal update; using owned run snapshot", err, log.String("run_id", r.ID.String()))
	}
	if err == nil && isTerminalRunState(saved.State) {
		required := r.Provenance.VerificationRequired
		*r = *saved
		if required {
			r.Provenance.VerificationRequired = true
		}
		return requiredCompletionError(r)
	}
	if r.Provenance.VerificationRequired {
		if outcome.StopReason == run.StopFinalAnswer && ctx.Err() == nil {
			verifyErr := e.finalizeRequiredCandidate(ctx, r, outcome)
			if verifyErr != nil && r.State == run.StateRunning {
				// A capture reply may be lost after its transaction commits. CAS prevents
				// a failure write from replacing that pending or committed result.
				e.failRun(ctx, r, r.AgentID, verifyErr, time.Time{})
				if current, readErr := e.store.GetRun(writeCtx, r.ID); readErr == nil {
					*r = *current
				}
				r.Output = ""
			}
			return verifyErr
		}
		outcome.Output = ""
		outcome.FinishReason = ""
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
	if err := e.persistRun(writeCtx, &next); err != nil {
		return fmt.Errorf("cortex: persist terminal outcome: %w", err)
	}
	*r = next
	e.emitTerminalOutcome(writeCtx, r)
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
	if r.State == run.StateCompleted && (!r.Provenance.VerificationRequired || (r.VerificationStatus == verification.StatusPassed && r.ResultRef != "")) {
		kind = EventDone
	}
	return StreamEvent{Type: kind, Data: map[string]any{
		"publication_status": string(r.Provenance.PublicationStatus),
		"run_id":             r.ID.String(), "output": r.Output, "tokens_used": r.TokensUsed,
		"state": string(r.State), "stop_reason": string(r.StopReason),
		"verification_status": string(r.VerificationStatus.Normalized()), "duration_ms": duration.Milliseconds(),
	}}
}

// emitTerminalOutcome is called once per run, after its terminal state is
// written, from every path that ends one. That makes it the place to count
// finished runs: the RunTerminated hook has the same once-only contract.
// It is also where a finished run's progress snapshot goes, since every
// path that ends a run passes through here once its write has landed.
func (e *Engine) emitTerminalOutcome(ctx context.Context, r *run.Run) {
	e.dropProgress(ctx, r)
	e.instr.runFinished(r)
	e.extensions.EmitRunTerminated(ctx, r.AgentID, r.ID, r.TerminalSummary())
	e.answerDirective(ctx, r)
}

// runErrorEvent shares safe verification failure metadata with HTTP and dashboard
// adapters. Other errors retain their existing checked message contract.
func runErrorEvent(r *run.Run, err error) StreamEvent {
	if failure, ok := run.DescribeVerificationFailure(err, r); ok {
		return StreamEvent{Type: EventError, Data: failure.Fields()}
	}
	return StreamEvent{Type: EventError, Data: map[string]any{"message": err.Error()}}
}
