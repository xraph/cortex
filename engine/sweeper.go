package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/suspension"
)

const (
	// defaultSuspensionTTL is how long a paused run waits for whoever
	// was supposed to answer it. A day is long enough for a human
	// approval to survive a weekend-adjacent afternoon and short enough
	// that a run abandoned by a closed browser tab does not sit in the
	// paused list for a quarter.
	defaultSuspensionTTL = 24 * time.Hour

	// defaultSweepInterval is how often the sweeper looks. Deadlines are
	// measured in hours, so a minute of granularity costs one small
	// indexed read per minute and buys expiry that lands close enough to
	// the deadline for anyone watching a run.
	defaultSweepInterval = time.Minute

	// defaultSweepLimit caps one sweep's batch. A restart after a long
	// outage can find thousands of rows past their deadline at once, and
	// a sweep that took all of them would hold the store for as long as
	// the backlog was long. Whatever is left over is picked up on the
	// next tick, which is one minute away.
	defaultSweepLimit = 100
)

// sweeper is the engine's expiry lifecycle. Engine.Start discards its
// context, so there is nothing there to cancel and the goroutine needs a
// handle of its own; done is what makes Engine.Stop a join rather than a
// signal.
type sweeper struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// startSweeper launches the expiry loop, unless expiry is off or there
// is no store to sweep. Calling it twice is a no-op: a second Start on a
// running engine must not leave two loops failing the same runs.
func (e *Engine) startSweeper() {
	if e.suspensionTTL <= 0 || e.store == nil {
		return
	}

	e.sweep.mu.Lock()
	defer e.sweep.mu.Unlock()
	if e.sweep.cancel != nil {
		return
	}

	// Background rather than Start's context: Start takes one and throws
	// it away, so a sweeper hung off it would either never stop or stop
	// the moment a caller's request context did.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	e.sweep.cancel = cancel
	e.sweep.done = done

	interval := e.sweepInterval
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				e.sweepOnce(ctx)
			}
		}
	}()
}

// stopSweeper cancels the loop and WAITS for it to exit. Signalling
// without waiting would let Stop return while a sweep was still writing,
// which is a run failed by an engine its host believes is shut down, and
// a data race in every test that stops an engine and then reads what it
// wrote.
func (e *Engine) stopSweeper() {
	e.sweep.mu.Lock()
	cancel, done := e.sweep.cancel, e.sweep.done
	e.sweep.cancel, e.sweep.done = nil, nil
	e.sweep.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// sweepOnce is one pass: find the suspensions nobody answered in time
// and fail their runs.
//
// One instant is read and then used for both the list and every claim
// below it. Taking a fresh time.Now per claim would let a row that the
// list found expired be claimed against a later instant, which is
// harmless here but would quietly diverge the two halves of a decision
// that has to agree.
func (e *Engine) sweepOnce(ctx context.Context) {
	if e.store == nil || e.suspensionTTL <= 0 {
		return
	}

	now := time.Now().UTC()
	// The one read in this codebase that deliberately crosses scopes.
	// The sweeper has no request scope and no way to enumerate the ones
	// that exist, so a scoped list would find nothing at all.
	expired, err := e.store.ListExpiredAcrossScopes(ctx, now, e.sweepLimit)
	if err != nil {
		e.logger.Error("list expired suspensions", log.String("error", err.Error()))
		return
	}

	for _, susp := range expired {
		if err := e.expireSuspension(ctx, susp, now); err != nil {
			e.logger.Error("expire suspension",
				log.String("run_id", susp.RunID.String()),
				log.String("error", err.Error()),
			)
		}
	}
}

// expireSuspension fails one run whose resumer never came back.
//
// It claims first, and the claim is the whole design rather than a
// precaution. A sweep is the third writer to these runs, after a resume
// and a rejected checkpoint, and Task 5 already paid for the version of
// this where one of the three skipped the claim: a rejection stomped a
// run an approval had moved. ClaimExpiredSuspension refuses a run that
// is no longer paused, so a resume that got in before the deadline keeps
// its run and the sweep leaves it alone.
//
// The race with a legitimate Resume resolves at the store, against the
// stored deadline, and it resolves the same way whichever order the two
// arrive in. A resume claims only an UNEXPIRED suspension; a sweep
// claims only an EXPIRED one. The two predicates partition the row, so
// there is no instant at which both believe it is theirs, and the
// resume's own claim leaves the run running, which is precisely what
// makes the sweep's claim miss. The reverse case is the one that would
// hurt more and it is closed the same way: once the sweep has claimed,
// a resume arriving a moment later finds no paused run and gets
// ErrNotSuspended rather than continuing a run about to be failed.
//
// The window a claim alone would not close is the interesting one. A
// resume that beat the deadline can still be executing when the deadline
// passes, because it holds the suspension row from ClaimSuspension all
// the way to its own DeleteSuspension, and an approved tool call runs in
// between. Such a row is still listed as expired, and this is exactly
// where an unclaimed sweeper would fail a run mid-flight. The claim
// sees the run is running and skips it.
func (e *Engine) expireSuspension(ctx context.Context, susp *suspension.Suspension, now time.Time) error {
	// Under the run's OWN scope, not the sweeper's, which has none.
	// Crossing scopes to find the work does not license doing the work
	// unscoped, and everything a run records has to land where its
	// earlier writes did.
	ctx = cortex.WithScope(ctx, susp.Scope)

	if _, err := e.store.ClaimExpiredSuspension(ctx, susp.RunID, now); err != nil {
		if errors.Is(err, cortex.ErrNotSuspended) {
			// Somebody else owns this run: it was resumed, decided, or
			// already swept. Not an error, and nothing to log.
			return nil
		}
		return fmt.Errorf("claim expired suspension: %w", err)
	}

	// From here the run belongs to this sweep and it is no longer
	// paused, so every exit below either fails it or says why it could
	// not. WithoutCancel because a sweep interrupted by Stop halfway
	// through must not leave a run running with nothing to resume it:
	// that is strictly worse than the paused row it started from, and it
	// is the same reason every other terminal write in this loop drops
	// its cancellation.
	ctx = context.WithoutCancel(ctx)

	r, err := e.store.GetRun(ctx, susp.RunID)
	if err != nil {
		return fmt.Errorf("load expired run: %w", err)
	}

	if err := e.persistFailure(ctx, r, r.AgentID, expiryError(susp)); err != nil {
		// The suspension stays. It is the only record of what the run
		// was waiting on, and the next sweep will find it again.
		return fmt.Errorf("fail expired run %s: %w", susp.RunID, err)
	}

	// The run is failed and no claim will ever take it again, so the row
	// is dead weight. Dropping it is also what makes the expiry final: a
	// resume after this finds nothing to claim.
	if err := e.store.DeleteSuspension(ctx, susp.RunID); err != nil && !errors.Is(err, cortex.ErrNotSuspended) {
		e.logger.Error("delete suspension of an expired run", log.String("error", err.Error()))
	}
	return nil
}

// expiryError is what the run's Error field ends up saying. It wraps
// cortex.ErrSuspensionExpired so a caller can match on it, and it names
// the deadline, because "expired" without a time sends whoever reads it
// looking for a log line to find out when.
func expiryError(susp *suspension.Suspension) error {
	deadline := "its deadline"
	if susp.ExpiresAt != nil {
		deadline = susp.ExpiresAt.Format(time.RFC3339)
	}
	return fmt.Errorf("%w: nothing answered the %s pending call(s) by %s",
		cortex.ErrSuspensionExpired, susp.Reason, deadline)
}
