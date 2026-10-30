package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/telemetry"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

const (
	// defaultSuspensionTTL is how long a paused run waits for whoever
	// was supposed to answer it. A day is long enough for a human
	// approval to survive a weekend-adjacent afternoon and short enough
	// that a run abandoned by a closed browser tab does not sit in the
	// paused list for a quarter.
	defaultSuspensionTTL = 24 * time.Hour

	// defaultAgentReplyGrace is how long past an ask's reply-by the
	// engine waits before failing the asking run. The ask sweep resolves
	// the ask at reply-by and resumes the run with a timeout, and five
	// minutes is ten of its default ticks, so the engine only fails a
	// run the bus could not resume.
	defaultAgentReplyGrace = 5 * time.Minute

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

	// checkpointCursor is where the stale checkpoint pass picks up: the
	// last checkpoint id it looked at, or nil to start from the first.
	cursorMu         sync.Mutex
	checkpointCursor id.CheckpointID
}

// startSweeper launches the expiry loop, unless there is no store to
// sweep. It runs even with a zero suspension TTL: that only drops the
// default deadline, and an approval, an external tool or an agent reply
// can still carry a deadline of its own. Calling it twice is a no-op: a
// second Start on a running engine must not leave two loops failing the
// same runs.
func (e *Engine) startSweeper() {
	if e.store == nil {
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

// sweepOnce is one pass of the sweeper, each part under the run's own
// scope as it acts on it:
//
//   - fail the runs whose suspension nobody answered in time;
//   - mark reclaimable the running runs whose worker's lease lapsed;
//   - tell the host again about runs left ready or reclaimable longer than
//     WithReadyRenotify;
//   - close pending checkpoints whose run has moved past them.
//
// One instant is read and then used for every list and every write below
// it. Taking a fresh time.Now per write would let a row a list found due
// be acted on against a later instant, which is harmless here but would
// quietly diverge the two halves of a decision that has to agree.
func (e *Engine) sweepOnce(ctx context.Context) {
	if e.store == nil {
		return
	}
	now := time.Now().UTC()
	e.sweepExpired(ctx, now)
	e.sweepLapsed(ctx, now)
	e.renotify(ctx, now)
	e.sweepStaleCheckpoints(ctx, now)
}

// sweepExpired finds the suspensions nobody answered in time and fails
// their runs.
func (e *Engine) sweepExpired(ctx context.Context, now time.Time) {
	// The sweeper has no request scope and no way to enumerate the ones
	// that exist, so a scoped list would find nothing at all: this read,
	// like the sweeper's other listings, deliberately crosses scopes.
	expired, err := e.store.ListExpiredAcrossScopes(ctx, now, e.sweepLimit)
	if err != nil {
		e.logError("list expired suspensions", err)
		return
	}

	// One line per sweep, not per row: a store outage fails every row in
	// the batch, and each one is already on its own expire suspension span.
	failed := 0
	var firstErr error
	for _, susp := range expired {
		if err := e.expireSuspension(ctx, susp, now); err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if failed > 0 {
		e.logError("expire suspensions", firstErr, log.Int("failed", failed), log.Int("batch", len(expired)))
	}
}

// sweepLapsed takes every running run whose worker's lease lapsed and
// marks it reclaimable, then fires RunReclaimable for it. A lease renewed
// since the list, or a run that left running, is skipped, not an error.
func (e *Engine) sweepLapsed(ctx context.Context, now time.Time) {
	if e.continuation == nil {
		return
	}
	lapsed, err := e.continuation.ListLapsedAcrossScopes(ctx, now, e.sweepLimit)
	if err != nil {
		e.logError("list runs whose lease lapsed", err)
		return
	}
	failed := 0
	var firstErr error
	for _, r := range lapsed {
		if r.Lease == nil {
			continue
		}
		previous := *r.Lease
		runCtx := cortex.WithScope(ctx, r.Scope)
		moved, markErr := e.continuation.MarkReclaimable(runCtx, r.ID, now, "")
		if errors.Is(markErr, cortex.ErrRunLeased) || errors.Is(markErr, cortex.ErrRunConflict) {
			continue
		}
		if markErr != nil {
			failed++
			if firstErr == nil {
				firstErr = markErr
			}
			continue
		}
		e.instr.reclaim(reclaimReclaimed)
		e.announceReclaimable(runCtx, moved, run.ReclaimNotice{PreviousOwner: previous.Owner, LeaseExpiredAt: previous.ExpiresAt, Reclaims: moved.Reclaims})
	}
	if failed > 0 {
		e.logError("mark lapsed runs reclaimable", firstErr, log.Int("failed", failed), log.Int("batch", len(lapsed)))
	}
}

// renotify tells the host again about every run left ready or reclaimable
// since before the WithReadyRenotify period, with Renotified set. Each run
// is stamped first, so it is repeated at most once per period whatever
// the hooks do. A run claimed since the list is skipped.
func (e *Engine) renotify(ctx context.Context, now time.Time) {
	if e.continuation == nil || e.readyRenotify <= 0 {
		return
	}
	waiting, err := e.continuation.ListContinuableAcrossScopes(ctx, now.Add(-e.readyRenotify), e.sweepLimit)
	if err != nil {
		e.logError("list runs waiting to continue", err)
		return
	}
	for _, r := range waiting {
		runCtx := cortex.WithScope(ctx, r.Scope)
		if markErr := e.continuation.MarkNotified(runCtx, r.ID, now); markErr != nil {
			if !errors.Is(markErr, cortex.ErrRunNotContinuable) {
				e.logWarn("record a repeated run notice", markErr, log.String("run_id", r.ID.String()))
			}
			continue
		}
		switch r.State {
		case run.StateReady:
			notice := run.ReadyNotice{Renotified: true}
			if r.Ready != nil {
				notice.Source, notice.CheckpointID, notice.At = r.Ready.Source, r.Ready.CheckpointID, r.Ready.At
			}
			e.extensions.EmitRunReady(runCtx, r.AgentID, r.ID, notice)
		case run.StateReclaimable:
			e.extensions.EmitRunReclaimable(runCtx, r.AgentID, r.ID, run.ReclaimNotice{LeaseExpiredAt: r.UpdatedAt, Reclaims: r.Reclaims, Renotified: true})
		}
	}
}

// sweepStaleCheckpoints closes pending checkpoints that no decision can
// act on any more, with the same superseded reason the decide path
// writes: a checkpoint whose run is waiting on a different pause, and one
// whose run has no pause at all and has not been written for longer than
// the grace (a run that ended, or carried on, without its decision being
// recorded). A run that is merely between a decider's action and its
// record is written moments before, so the grace leaves it alone.
//
// It walks the pending checkpoints a batch per sweep, picking up where the
// last batch ended, so checkpoints still legitimately waiting on a person
// cannot keep the rest out of reach. Only the engine's own checkpoints are
// touched: one a host wrote itself is the host's to close.
func (e *Engine) sweepStaleCheckpoints(ctx context.Context, now time.Time) {
	sweep := e.checkpointSweep
	if sweep == nil || e.continuation == nil {
		return
	}
	e.sweep.cursorMu.Lock()
	defer e.sweep.cursorMu.Unlock()
	pending, err := sweep.ListPendingAcrossScopes(ctx, e.sweep.checkpointCursor, e.sweepLimit)
	if err != nil {
		e.logError("list pending checkpoints", err)
		return
	}
	if len(pending) < e.sweepLimit {
		e.sweep.checkpointCursor = id.CheckpointID{}
	} else {
		e.sweep.checkpointCursor = pending[len(pending)-1].ID
	}
	for _, cp := range pending {
		if !openedASuspendedRun(cp) {
			continue
		}
		cpCtx := cortex.WithScope(ctx, cp.Scope)
		if e.checkpointStale(cpCtx, cp, now) {
			e.closeCheckpoint(cpCtx, cp, supersededReason)
		}
	}
}

// checkpointStale reports whether a decision on cp can no longer act on
// anything. A read that fails says no: the next sweep looks again.
func (e *Engine) checkpointStale(ctx context.Context, cp *checkpoint.Checkpoint, now time.Time) bool {
	r, err := e.store.GetRun(ctx, cp.RunID)
	if errors.Is(err, cortex.ErrRunNotFound) {
		return true
	}
	if err != nil {
		return false
	}
	susp, err := e.store.GetSuspension(ctx, cp.RunID)
	if err == nil {
		opened, ok := cp.Metadata[checkpointSuspensionKey].(string)
		return !ok || opened != susp.ID.String()
	}
	if !errors.Is(err, cortex.ErrNotSuspended) {
		return false
	}
	if r.State == run.StateReady && r.Ready != nil && r.Ready.CheckpointID == cp.ID {
		return false
	}
	return r.UpdatedAt.Before(now.Add(-e.idleGrace()))
}

// expireSuspension fails one run whose resumer never came back.
//
// It claims first, and the claim is the whole design rather than a
// precaution. A sweep is the third writer to these runs, after a resume
// and a rejected checkpoint, and skipping the claim has already cost a
// real bug: without it, a rejection stomped a run an approval had
// moved. ClaimExpiredSuspension refuses a run that
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
//
// Each expired row is its own root expire suspension span. The sweep's tick
// and its list query start no trace, so a quiet minute costs nothing.
func (e *Engine) expireSuspension(ctx context.Context, susp *suspension.Suspension, now time.Time) error {
	reason := suspendReasonLabel(susp.Reason)
	ctx, span := telemetry.Tracer().Start(ctx, spanExpireSuspension, trace.WithNewRoot(), trace.WithAttributes(
		attribute.String(attrRunID, susp.RunID.String()),
		attribute.String(attrSuspensionReason, reason),
	))
	defer span.End()

	outcome, err := e.expireOne(ctx, susp, now)
	span.SetAttributes(attribute.String(attrSuspensionOutcome, outcome))
	switch {
	case err != nil:
		telemetry.Fail(span, err)
		if e.instr != nil {
			e.instr.expired.Inc(reason, resultError)
		}
	case outcome == expiryExpired && e.instr != nil:
		e.instr.expired.Inc(reason, resultOK)
	}
	return err
}

// How one expired row ended up, for the expire suspension span.
const (
	// expiryExpired: the run was failed and the row dropped.
	expiryExpired = "expired"
	// expirySkipped: somebody else owns the run (resumed, decided or
	// already swept), so the row was left alone.
	expirySkipped = "skipped"
	// expiryOrphan: the run had already ended or was gone, and only the
	// leftover row was cleared.
	expiryOrphan = "orphan_dropped"
	// expiryError: something failed; the next sweep tries again.
	expiryError = "error"
)

func (e *Engine) expireOne(ctx context.Context, susp *suspension.Suspension, now time.Time) (string, error) {
	// Under the run's OWN scope, not the sweeper's, which has none.
	// Crossing scopes to find the work does not license doing the work
	// unscoped, and everything a run records has to land where its
	// earlier writes did.
	ctx = cortex.WithScope(ctx, susp.Scope)

	// Before the claim, same as every other claimant: a read that fails
	// after one leaves the run running with no way back, and the next
	// sweep can no longer touch it. Failing here instead costs one tick.
	snapshot, err := e.store.GetRun(ctx, susp.RunID)
	if err != nil {
		if errors.Is(err, cortex.ErrRunNotFound) {
			// No run to fail. The row is all that is left of it.
			return orphanOutcome(e.deleteOrphanRow(ctx, susp))
		}
		return expiryError, fmt.Errorf("load the run behind an expired suspension: %w", err)
	}

	// Under the lease when this engine can take it with the claim: a sweep
	// that dies before its failure write leaves a lease that lapses, not a
	// running run nobody owns.
	// The snapshot carries the expiry, so a worker that reclaims the run
	// after a lost failure write fails it the same way.
	first := claimSnapshot(susp, nil)
	first.Ending = &suspension.Ending{Kind: suspension.EndingExpire, Reason: expiredDetail(susp)}
	claimed, leased, err := e.claimPauseLeased(ctx, snapshot, susp, now, first)
	if !leased {
		claimed, err = e.store.ClaimExpiredSuspension(ctx, susp.RunID, now)
	}
	if err != nil {
		if errors.Is(err, cortex.ErrNotSuspended) || errors.Is(err, cortex.ErrRunConflict) {
			// Somebody else owns this run: it was resumed, decided, or
			// already swept. Not an error, and nothing to log.
			dropped, dropErr := e.dropOrphanedSuspension(ctx, susp)
			if dropErr != nil {
				return expiryError, dropErr
			}
			if dropped {
				return expiryOrphan, nil
			}
			return expirySkipped, nil
		}
		return expiryError, fmt.Errorf("claim expired suspension: %w", err)
	}

	// From here the run belongs to this sweep and it is no longer
	// paused, so every exit below either fails it or says why it could
	// not. WithoutCancel because a sweep interrupted by Stop halfway
	// through must not leave a run running with nothing to resume it:
	// that is strictly worse than the paused row it started from, and it
	// is the same reason every other terminal write in this loop drops
	// its cancellation.
	ctx = context.WithoutCancel(ctx)

	r := e.claimedRun(ctx, susp.RunID, snapshot, claimed.ClaimedRun)

	if err := e.persistFailure(ctx, r, r.AgentID, expiredRunError(susp)); err != nil {
		// The suspension stays. It is the only record of what the run
		// was waiting on. A leased claim's lease lapses and the run is
		// reclaimed, and the expiry its snapshot carries is finished then;
		// a claim made without one leaves the run for the next sweep.
		return expiryError, fmt.Errorf("fail expired run %s: %w", susp.RunID, err)
	}

	// The run is failed and no claim will ever take it again, so the row
	// is dead weight. Dropping it is also what makes the expiry final: a
	// resume after this finds nothing to claim. A failure here is logged
	// rather than returned, because the run really did fail and that is
	// what the caller cares about; the leftover row is picked up by
	// dropOrphanedSuspension on the next sweep.
	if err := e.store.DeleteSuspension(ctx, susp.RunID); err != nil && !errors.Is(err, cortex.ErrNotSuspended) {
		e.logError("delete suspension of an expired run", err, log.String("run_id", susp.RunID.String()))
	}

	// An approval pause opened a checkpoint, and the run it belongs to
	// has just been failed. Left pending, the row sits in ListPending
	// asking somebody to decide a run that ended hours ago, and there is
	// no suspension behind it any more, so whoever does decide it gets an
	// error out of the claim. Resolved here, the queue only ever holds
	// decisions that can still change something.
	e.closePendingCheckpoints(ctx, susp.RunID, "nobody answered in time, so the run was failed and there is nothing left to decide")
	return expiryExpired, nil
}

// orphanOutcome reports a row cleared because its run was gone.
func orphanOutcome(err error) (string, error) {
	if err != nil {
		return expiryError, err
	}
	return expiryOrphan, nil
}

// dropOrphanedSuspension clears a row whose run has already finished.
//
// A sweep that fails a run and then cannot delete its suspension leaves
// the row behind, and it is permanently unclaimable: the claim wants a
// paused run and this one is failed. Every later sweep re-lists it,
// refuses it and moves on, so it occupies a slot of the batch forever.
// A handful of those across a fleet slowly starve the sweep of room for
// runs that still need failing. The same is true of the row suspend
// leaves behind when its rollback delete fails.
//
// The rule is narrow on purpose: only a run in a TERMINAL state. A
// resume in flight leaves its run running, never terminal, so this can
// never take a row out from under one. Nothing can resume a terminal run
// either, since every claim needs a paused one, which makes the row pure
// dead weight rather than a record anybody could still act on.
//
// Everything else the refused claim could mean (the run is paused again,
// running, or the row is simply gone) is left exactly as found.
//
// It reports whether it dropped the row.
func (e *Engine) dropOrphanedSuspension(ctx context.Context, susp *suspension.Suspension) (bool, error) {
	r, err := e.store.GetRun(ctx, susp.RunID)
	if err != nil {
		if errors.Is(err, cortex.ErrRunNotFound) {
			// No run to be orphaned from. Dropping the row is the only
			// thing left that can ever be done with it.
			return true, e.deleteOrphanRow(ctx, susp)
		}
		return false, fmt.Errorf("load run behind a refused claim: %w", err)
	}
	if !isTerminalRunState(r.State) {
		return false, nil
	}
	return true, e.deleteOrphanRow(ctx, susp)
}

func (e *Engine) deleteOrphanRow(ctx context.Context, susp *suspension.Suspension) error {
	// WithoutCancel for the same reason the terminal write uses it: this
	// is cleanup of a run that has already ended, and half-doing it just
	// leaves the next sweep the same work.
	if err := e.store.DeleteSuspension(context.WithoutCancel(ctx), susp.RunID); err != nil &&
		!errors.Is(err, cortex.ErrNotSuspended) {
		return fmt.Errorf("delete orphaned suspension: %w", err)
	}
	return nil
}

// isTerminalRunState says whether a run has finished for good. Written
// as an allowlist rather than "not paused and not running", so a state
// added later is treated as live until somebody decides otherwise: the
// cost of being wrong the other way is deleting a row a run still needs.
func isTerminalRunState(s run.State) bool {
	switch s {
	case run.StateCompleted, run.StateIncomplete, run.StateFailed, run.StateCancelled:
		return true
	case run.StateCreated, run.StateRunning, run.StatePaused, run.StateReady, run.StateReclaimable:
		return false
	default:
		return false
	}
}

// expiredRunError is what the run's Error field ends up saying. It wraps
// cortex.ErrSuspensionExpired so a caller can match on it, and it names
// the deadline, because "expired" without a time sends whoever reads it
// looking for a log line to find out when.
func expiredRunError(susp *suspension.Suspension) error {
	return fmt.Errorf("%w: %s", cortex.ErrSuspensionExpired, expiredDetail(susp))
}

// expiredDetail is what expiredRunError says after the sentinel.
func expiredDetail(susp *suspension.Suspension) string {
	deadline := "its deadline"
	if susp.ExpiresAt != nil {
		deadline = susp.ExpiresAt.Format(time.RFC3339)
	}
	return fmt.Sprintf("nothing answered the %s pending call(s) by %s", susp.Reason, deadline)
}
