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
		reason := fmt.Sprintf("no reply from %s before the deadline", a.Expected)
		if err := b.resolveAskWithFailure(ctx, a.ReplyWith, reason); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
