package a2a

import (
	"context"
	"fmt"
)

// sweepBatch caps one pass so a large backlog cannot hold a worker forever.
const sweepBatch = 100

// SweepExpiredAsks resolves every ask whose deadline has passed into a
// timeout failure and resumes the run waiting on it, returning how many it
// resolved.
//
// The engine's own suspension sweep FAILS a run nobody answered in time.
// For an agent-reply pause that is the wrong verb, so this runs first: a
// peer that did not answer is something the asking agent can react to, and
// killing the run throws that away. The engine sweep stays as the outer
// backstop for anything this missed.
func (b *Bus) SweepExpiredAsks(ctx context.Context) (int, error) {
	asks, err := b.store.ListExpiredAsks(ctx, b.clock.Now(), sweepBatch)
	if err != nil {
		return 0, err
	}
	var n int
	for _, a := range asks {
		if err := b.resolveAskWithFailure(ctx, a.ReplyWith, b.timeoutReason(ctx, a)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// timeoutReason phrases an overdue ask for the agent that has to read it.
//
// Naming one agent is right when only one was asked and wrong when
// several were: an initiator whose tender timed out with two of three
// bids in hand should not be told that a particular agent went quiet, as
// though it were the only one it was waiting on.
func (b *Bus) timeoutReason(ctx context.Context, a *PendingAsk) string {
	if a.MessageID.IsNil() {
		return "no reply before the deadline"
	}
	e, err := b.store.GetMessage(ctx, a.MessageID)
	if err != nil || len(e.Receivers) <= 1 {
		return fmt.Sprintf("no reply from %s before the deadline", a.Expected)
	}
	return "not every agent answered before the deadline"
}
