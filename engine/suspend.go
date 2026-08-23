package engine

import (
	"context"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

// pendingCall records one tool call the loop could not finish itself.
//
// Arguments travel with it rather than being left in the continuation's
// last assistant message. Whoever resumes an external call has to
// actually execute it, and making them match call ids against the
// message history to find out with what would be a puzzle with exactly
// one consumer.
func pendingCall(tc llm.ToolCall) suspension.PendingCall {
	return suspension.PendingCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments}
}

// suspend stops a run that cannot go on until something outside the
// engine answers, and it is the only place in the loop that writes a
// suspension. Reason is a parameter because the loop does not care why a
// call is pending, only that it is: an external tool today, a call
// escalated for human approval next, both landing here rather than each
// growing its own suspend path.
//
// The suspension is written before the run flips to paused, and the order
// is load-bearing. If the run went paused first and the suspension write
// then failed, the run would be paused with nothing to resume from and no
// way back: ClaimSuspension only ever finds a suspension for a paused
// run. This way round the worst case is a suspension attached to a run
// that is still marked running, which Claim refuses and the caller sees
// as a failed run rather than a wedged one.
//
// Nothing is saved to conversation memory here. The continuation carries
// the messages, and Resume is what eventually persists them: saving now
// would have the resumed run reload its own in-flight messages as
// history and answer them twice.
func (e *Engine) suspend(ctx context.Context, r *run.Run, reason suspension.SuspendReason, pending []suspension.PendingCall, cont suspension.Continuation) error {
	s := &suspension.Suspension{
		Entity:  cortex.NewEntity(),
		ID:      id.NewSuspensionID(),
		RunID:   r.ID,
		Scope:   r.Scope,
		Reason:  reason,
		Pending: pending,
		Cont:    cont,
	}
	if err := e.store.CreateSuspension(ctx, s); err != nil {
		return fmt.Errorf("create suspension: %w", err)
	}

	r.State = run.StatePaused
	r.StepCount = cont.StepIndex
	r.TokensUsed = cont.TokensUsed
	if err := e.store.UpdateRun(ctx, r); err != nil {
		return fmt.Errorf("update run to paused: %w", err)
	}
	return nil
}
