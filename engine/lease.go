package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

// defaultRunLease is how long a lease lasts when WithRunLease is not set.
const defaultRunLease = 60 * time.Second

// leaseWriteTimeout bounds the store writes a lease makes on a context
// that no longer carries the run's cancellation.
const leaseWriteTimeout = 5 * time.Second

// leaseHoldAttempts is how many times a pass tries to take the lease of a
// run it just claimed from a pause before it gives the run up.
const leaseHoldAttempts = 3

var (
	// errProgressUnsaved stops a pass whose Started record could not be
	// saved: the call is not dispatched and nothing more is written, so the
	// lease lapses and another replica reclaims the run.
	errProgressUnsaved = fmt.Errorf("cortex: a started call could not be recorded: %w", cortex.ErrRunInterrupted)
	// errChildInterrupted stops a pass whose tool reported that a run it
	// started was handed off or lost its lease.
	errChildInterrupted = fmt.Errorf("cortex: a run started by a tool stopped before it finished: %w", cortex.ErrRunInterrupted)
	// errParentStopped is what a nested run sees when the run it was
	// started from was interrupted. Its own lease is still good, so it is
	// handed off under its own name.
	errParentStopped = fmt.Errorf("cortex: the run this one was started from stopped: %w", cortex.ErrRunInterrupted)
)

// WorkerID returns the name this engine holds run leases under. See
// WithWorkerID.
func (e *Engine) WorkerID() string { return e.workerID }

// defaultWorkerID is "<hostname>-<pid>-<6 random hex digits>": the host
// and process say where a lease came from, and the random part keeps two
// engines in one process, or a restarted process that got the same pid,
// apart.
func defaultWorkerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "cortex"
	}
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms; the time keeps
		// the id distinct if it ever does.
		return fmt.Sprintf("%s-%d-%06x", host, os.Getpid(), time.Now().UnixNano()&0xffffff)
	}
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}

// leasesRun reports whether r executes under a lease on this engine. It is
// decided once per run, when the run is created or claimed, and the
// answer holds for that whole pass: a run claimed without a lease is
// never heartbeated, because HoldLease would take over a running run
// that has no owner.
func (e *Engine) leasesRun(r *run.Run) bool {
	return e.leaseTTL > 0 && e.continuation != nil && e.progress != nil &&
		!r.Provenance.VerificationRequired && r.Provenance.TaskClaimID == ""
}

// newLease is the lease a run is created or claimed under now.
func (e *Engine) newLease() run.Lease {
	return run.Lease{Owner: e.workerID, ExpiresAt: time.Now().UTC().Add(e.leaseTTL)}
}

// runLease is one pass's hold on a leased run: the heartbeat that renews
// it, the cancellation that stops the run when it is lost or handed off,
// and the progress the run has saved since its last boundary.
type runLease struct {
	e        *Engine
	runID    id.AgentRunID
	agentID  id.AgentID
	scope    cortex.Scope
	owner    string
	reclaims int
	// base carries the run's scope and none of its cancellation, for the
	// writes a lease makes after the run's context has ended.
	base   context.Context
	cancel context.CancelCauseFunc
	// nested is true for a run started from inside another run's call (a
	// tool calling RunAgent on its ctx). It does not hand off because the
	// engine drains: it ends with the call that started it.
	nested bool
	// cause is why this lease's own context was cancelled, as opposed to a
	// cause inherited from a parent run's context.
	cause atomic.Pointer[error]

	quit    chan struct{}
	done    chan struct{}
	quiesce sync.Once

	mu       sync.Mutex
	progress progressState
}

type runLeaseKey struct{}

// leaseFor returns the lease the context holds on runID, or nil. A run
// started from inside a leased run's tool inherits its parent's context,
// so the run id is what tells the parent's lease apart from its own.
func leaseFor(ctx context.Context, runID id.AgentRunID) *runLease {
	l, ok := ctx.Value(runLeaseKey{}).(*runLease)
	if !ok || l == nil || l.runID != runID {
		return nil
	}
	return l
}

// holdLease starts renewing r's lease, which the caller has already
// written (CreateRun, HoldLease or ClaimContinuation), every ttl/3. It
// returns the context the run must execute on. The loop stops the
// heartbeat when it exits (releaseLease); the context itself ends when the
// admission ticket of the call that ran the run is released, since some
// callers go on using it after the loop.
//
// On cortex.ErrRunLeaseLost the heartbeat cancels the context with that
// cause and stops. The loop, seeing it, returns at its next boundary
// without a terminal write. Stop cancels it with cortex.ErrEngineStopping
// when it runs out of time, and the loop hands the run off.
//
// The release is also tied to the run's admission ticket, so the lease
// ends when the call that runs the run returns, whichever path it took.
func (e *Engine) holdLease(ctx context.Context, r *run.Run) context.Context {
	runCtx, cancel := context.WithCancelCause(ctx)
	l := &runLease{
		e: e, runID: r.ID, agentID: r.AgentID, scope: r.Scope, owner: e.workerID, reclaims: r.Reclaims,
		base:   cortex.WithScope(context.WithoutCancel(ctx), r.Scope),
		cancel: cancel,
		nested: startedInsideARun(ctx, r.ID),
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	runCtx = context.WithValue(runCtx, runLeaseKey{}, l)
	e.leases.add(l)
	every := e.leaseTTL / 3
	if every < time.Millisecond {
		every = time.Millisecond
	}
	go l.heartbeat(every)
	if ticket, ok := ctx.Value(admissionTicketKey{}).(*admissionTicket); ok {
		ticket.onRelease(l.release)
	}
	return runCtx
}

// startedInsideARun reports whether ctx belongs to another run's call:
// it carries that run's lease, or an admission ticket that let this run in
// under its parent's slot.
func startedInsideARun(ctx context.Context, runID id.AgentRunID) bool {
	if parent, ok := ctx.Value(runLeaseKey{}).(*runLease); ok && parent != nil && parent.runID != runID {
		return true
	}
	ticket, ok := ctx.Value(admissionTicketKey{}).(*admissionTicket)
	return ok && !ticket.holdSlot
}

// interrupt cancels this lease's run with cause, and remembers the cause
// as the run's own. The first cause wins.
func (l *runLease) interrupt(cause error) {
	l.cause.CompareAndSwap(nil, &cause)
	l.cancel(cause)
}

// ownCause is the cause this lease's run was interrupted with, or nil.
func (l *runLease) ownCause() error {
	if c := l.cause.Load(); c != nil {
		return *c
	}
	return nil
}

// releaseLease stops renewing the lease ctx holds on runID, if any. The
// loops defer it, so a pass that ends any way at all stops renewing. The
// run's context is left alone, since some callers go on using it after the
// loop; it ends with the admission ticket of the call that ran the run.
func releaseLease(ctx context.Context, runID id.AgentRunID) {
	if l := leaseFor(ctx, runID); l != nil {
		l.stop()
	}
}

// leaseClaimed takes a lease on a run this call just claimed from a pause
// the old way, which left it running with no owner: a store without
// suspension.LeasedClaimStore, or an agent-reply resume started on another
// worker than the one that admitted it. A run this engine does not lease
// is returned as it is.
//
// A failed hold is retried a few times within a small part of the lease
// ttl. When every attempt fails the pass ends there, with
// cortex.ErrRunInterrupted and nothing written: a leasable run must not
// execute without its lease. A hold refused because another worker owns
// the run ends the pass with cortex.ErrRunLeaseLost; that worker's lease
// lapses if it is gone, and the sweep reclaims the run.
func (e *Engine) leaseClaimed(ctx context.Context, r *run.Run) (context.Context, error) {
	if r == nil || !e.leasesRun(r) {
		return ctx, nil
	}
	pause := e.leaseTTL / 30
	var err error
	for attempt := 1; ; attempt++ {
		lease := e.newLease()
		err = e.continuation.HoldLease(cortex.WithScope(ctx, r.Scope), r.ID, lease)
		if err == nil {
			r.Lease = &lease
			return e.holdLease(ctx, r), nil
		}
		if errors.Is(err, cortex.ErrRunLeaseLost) || attempt == leaseHoldAttempts || !waitFor(ctx, pause*time.Duration(attempt)) {
			break
		}
	}
	e.logError("hold the lease of a claimed run; leaving it for the sweep", err, log.String("run_id", r.ID.String()))
	if errors.Is(err, cortex.ErrRunLeaseLost) {
		return ctx, fmt.Errorf("cortex: run %s: %w", r.ID, err)
	}
	return ctx, fmt.Errorf("cortex: take the lease of run %s: %w: %w", r.ID, cortex.ErrRunInterrupted, err)
}

// leasedClaims returns the store's leased claims when this engine leases r:
// the claim then takes r's lease in the same transaction that moves it to
// running, so a claimer that dies leaves a lease that lapses. ok is false
// when the caller must claim the old way: leases off, a run this engine
// never leases, or a store without suspension.LeasedClaimStore.
func (e *Engine) leasedClaimsFor(r *run.Run) (suspension.LeasedClaimStore, bool) {
	if e.leasedClaims == nil || r == nil || !e.leasesRun(r) {
		return nil, false
	}
	return e.leasedClaims, true
}

// claimPauseLeased claims susp, the pause snapshot was read with, under a
// new lease of this worker and with first as the run's first progress
// snapshot. expiredBy set claims only a pause past its deadline. ok is
// false when this engine cannot take the lease with the claim.
func (e *Engine) claimPauseLeased(ctx context.Context, snapshot *run.Run, susp *suspension.Suspension, expiredBy time.Time, first *suspension.Progress) (claimed *suspension.Suspension, ok bool, err error) {
	store, leased := e.leasedClaimsFor(snapshot)
	if !leased || susp == nil {
		return nil, false, nil
	}
	lease := e.newLease()
	if first != nil {
		first.Owner = lease.Owner
	}
	claimed, err = store.ClaimSuspensionLeased(ctx, suspension.LeasedClaim{
		Version:   suspension.ClaimVersion{RunID: susp.RunID, SuspensionID: susp.ID, RunRevision: snapshot.Revision},
		ExpiredBy: expiredBy,
		Lease:     lease,
		First:     first,
	})
	return claimed, true, err
}

// claimSnapshot is the progress a run claimed from paused starts with:
// the state it paused with, every pending call the claim answers recorded
// as done with its answer, and every other call that already left the
// engine (a question to a peer, an external call handed to the host)
// recorded as started. A worker that reclaims the run then never sends a
// call twice: an answered one gets its answer, and one with no answer is
// uncertain unless its tool is retry-safe.
func claimSnapshot(paused *suspension.Suspension, done []suspension.ProgressResult) *suspension.Progress {
	answered := make(map[string]bool, len(done))
	for _, d := range done {
		answered[d.CallID] = true
	}
	var started []suspension.PendingCall
	for _, call := range sentCalls(paused) {
		if !answered[call.ID] {
			started = append(started, call)
		}
	}
	return &suspension.Progress{RunID: paused.RunID, Cont: pausedCont(paused.Cont), Done: done, Started: started}
}

// answeredCalls records each pending call that results answer as done,
// with the result the model will see: what a resume records for it before
// its publication checks, which the run's next pass repeats. A call the
// answer asks the engine to execute is not answered yet.
func answeredCalls(pending []suspension.PendingCall, in ResumeInput) []suspension.ProgressResult {
	byID := make(map[string]ToolResult, len(in.ToolResults))
	for _, res := range in.ToolResults {
		byID[res.ToolCallID] = res
	}
	var done []suspension.ProgressResult
	for _, p := range pending {
		res, ok := byID[p.ID]
		if !ok || res.Execute {
			continue
		}
		switch {
		case res.Error != "" && in.rejected:
			done = append(done, suspension.ProgressResult{CallID: p.ID, Content: jsonResult("error", res.Error), Error: res.Error, Outcome: progressOutcome(outcomeDenied)})
		case res.Error != "":
			done = append(done, suspension.ProgressResult{CallID: p.ID, Content: jsonResult("error", res.Error), Error: res.Error, Outcome: progressOutcome(outcomeFailed)})
		default:
			done = append(done, suspension.ProgressResult{CallID: p.ID, Content: res.Content, Outcome: progressOutcome(outcomeCompleted)})
		}
	}
	return done
}

// adoptLease starts holding the lease a leased claim took for this worker,
// with the snapshot the claim wrote as the pass's progress. ok is false
// when the claimed run is not under this worker's lease.
func (e *Engine) adoptLease(ctx context.Context, claimed *run.Run, first *suspension.Progress) (context.Context, bool) {
	if claimed == nil || claimed.Lease == nil || claimed.Lease.Owner != e.workerID || !e.leasesRun(claimed) {
		return ctx, false
	}
	ctx = e.holdLease(ctx, claimed)
	if first == nil && e.progress != nil {
		if saved, err := e.progress.GetProgress(cortex.WithScope(ctx, claimed.Scope), claimed.ID); err == nil {
			first = saved
		}
	}
	if l := leaseFor(ctx, claimed.ID); l != nil && first != nil {
		l.mu.Lock()
		l.progress = progressState{cont: first.Cont, started: slices.Clone(first.Started), done: slices.Clone(first.Done)}
		l.mu.Unlock()
	}
	return ctx, true
}

// waitFor sleeps for d, or until ctx ends, and reports whether it slept
// the whole time.
func waitFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (l *runLease) heartbeat(every time.Duration) {
	defer close(l.done)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-l.quit:
			return
		case <-ticker.C:
		}
		if l.e.renewalsPaused.Load() {
			continue
		}
		if err := l.renew(); errors.Is(err, cortex.ErrRunLeaseLost) {
			l.interrupt(cortex.ErrRunLeaseLost)
			return
		}
	}
}

// renew extends the lease by the engine's ttl from now.
func (l *runLease) renew() error {
	ctx, cancel := context.WithTimeout(l.base, leaseWriteTimeout)
	defer cancel()
	err := l.e.continuation.HoldLease(ctx, l.runID, run.Lease{Owner: l.owner, ExpiresAt: time.Now().UTC().Add(l.e.leaseTTL)})
	if err != nil && !errors.Is(err, cortex.ErrRunLeaseLost) {
		l.e.logWarn("renew a run lease", err, log.String("run_id", l.runID.String()))
	}
	return err
}

// release stops the heartbeat and ends the run's context.
func (l *runLease) release() {
	l.stop()
	l.cancel(context.Canceled)
}

// stop ends the heartbeat and waits for it, so no renewal is in flight
// once it returns, and takes the run out of Stop's reach. The run's
// context stays as it was.
func (l *runLease) stop() {
	l.quiesce.Do(func() {
		close(l.quit)
		<-l.done
		l.e.leases.remove(l)
	})
}

// interruption says why a leased run must stop where it is. Its own
// causes come first: cortex.ErrRunLeaseLost when its lease is gone,
// cortex.ErrEngineStopping when Stop ran out of time, errProgressUnsaved
// when a started call could not be recorded, errChildInterrupted when a
// run its tool started stopped. A nested run whose parent was interrupted
// sees errParentStopped, never the parent's own cause, so it is not taken
// for a run that lost its own lease. It is nil for any other run, and for
// a run whose context ended for another reason (its deadline, its caller).
func interruption(ctx context.Context, runID id.AgentRunID) error {
	l := leaseFor(ctx, runID)
	if l == nil {
		return nil
	}
	if own := l.ownCause(); own != nil {
		return own
	}
	if ctx.Err() == nil {
		return nil
	}
	inherited := context.Cause(ctx)
	if errors.Is(inherited, cortex.ErrRunLeaseLost) || errors.Is(inherited, cortex.ErrEngineStopping) || errors.Is(inherited, cortex.ErrRunInterrupted) {
		return errParentStopped
	}
	return nil
}

// handOffDue reports whether a leased run should stop at this boundary:
// the run was interrupted, or the engine is draining and the run is not
// nested inside another run's call.
func (e *Engine) handOffDue(ctx context.Context, runID id.AgentRunID) bool {
	l := leaseFor(ctx, runID)
	if l == nil {
		return false
	}
	if interruption(ctx, runID) != nil {
		return true
	}
	return !l.nested && e.admission.draining()
}

// interruptedByTool stops a leased run whose tool reported that a run it
// started was handed off or lost its lease. Nothing about that call is
// the run's outcome: it stays started, and the run hands off.
func interruptedByTool(ctx context.Context, runID id.AgentRunID, err error) {
	if !errors.Is(err, cortex.ErrRunInterrupted) && !errors.Is(err, cortex.ErrRunLeaseLost) {
		return
	}
	if l := leaseFor(ctx, runID); l != nil {
		l.interrupt(errChildInterrupted)
	}
}

// leaveRun ends this pass over a leased run without ending the run.
//
// A run whose lease is lost is left alone: another worker owns it now.
// Any other run is handed off: its progress is saved (fresh, when the
// caller is at a clean boundary; otherwise the snapshot from its last
// boundary stands), it is marked reclaimable under this worker's lease,
// and RunReclaimable fires with HandOff set. It returns
// cortex.ErrRunInterrupted for a hand-off, cortex.ErrRunLeaseLost for a
// lost lease, or nil when the run turns out to have ended already, in
// which case r is the ended run.
func (e *Engine) leaveRun(ctx context.Context, r *run.Run, fresh *suspension.Continuation) error {
	l := leaseFor(ctx, r.ID)
	if l == nil {
		return nil
	}
	l.stop()
	cause := interruption(ctx, r.ID)
	switch {
	case errors.Is(cause, cortex.ErrRunLeaseLost):
		return e.loseRun(l, r)
	case errors.Is(cause, errProgressUnsaved):
		// The store would not take the Started record, so it will not take
		// a hand-off either. Nothing is written and the lease lapses.
		e.logError("record a started call; leaving the run for reclaim", cause, log.String("run_id", r.ID.String()))
		return fmt.Errorf("cortex: run %s: %w", r.ID, cause)
	case errors.Is(cause, errParentStopped):
		e.logWarn("hand off a nested run whose parent stopped", cause, log.String("run_id", r.ID.String()))
	}
	if fresh != nil {
		if err := l.saveBoundary(*fresh); errors.Is(err, cortex.ErrRunLeaseLost) {
			return e.loseRun(l, r)
		}
	}
	writeCtx, cancel := context.WithTimeout(l.base, leaseWriteTimeout)
	defer cancel()
	at := time.Now().UTC()
	moved, err := e.continuation.MarkReclaimable(writeCtx, r.ID, at, l.owner)
	if errors.Is(err, cortex.ErrRunLeaseLost) || errors.Is(err, cortex.ErrRunConflict) {
		return e.loseRun(l, r)
	}
	if err != nil {
		// The run stays running under a lease nobody renews, so it lapses
		// and the sweeper reclaims it instead.
		e.logError("hand off a run", err, log.String("run_id", r.ID.String()))
		return fmt.Errorf("cortex: hand off run %s: %w: %w", r.ID, cortex.ErrRunInterrupted, err)
	}
	*r = *moved
	e.announceReclaimable(l.base, r, run.ReclaimNotice{
		PreviousOwner:  l.owner,
		LeaseExpiredAt: at,
		Reclaims:       r.Reclaims,
		HandOff:        true,
	})
	return cortex.ErrRunInterrupted
}

// announceReclaimable tells RunReclaimable subscribers, under the run's own
// scope, and records the notice so the sweeper repeats it only once the
// run has waited a whole WithReadyRenotify period since.
func (e *Engine) announceReclaimable(ctx context.Context, r *run.Run, notice run.ReclaimNotice) {
	runCtx := cortex.WithScope(ctx, r.Scope)
	e.extensions.EmitRunReclaimable(runCtx, r.AgentID, r.ID, notice)
	e.markNotified(runCtx, r.ID, time.Now().UTC())
}

// loseRun is the end of a pass whose lease went to someone else. Nothing
// is written. A run that has ended in the meantime while still this
// worker's (an operator cancelled it, say) is handed back as it ended, the
// way finalizeRun adopts a terminal state it finds. A run another worker
// reclaimed and finished is not this pass's outcome: its reclaim count
// has moved past the one this lease was taken at, and the pass reports
// cortex.ErrRunLeaseLost.
func (e *Engine) loseRun(l *runLease, r *run.Run) error {
	readCtx, cancel := context.WithTimeout(l.base, leaseWriteTimeout)
	defer cancel()
	if saved, err := e.store.GetRun(readCtx, r.ID); err == nil && isTerminalRunState(saved.State) && saved.Reclaims == l.reclaims {
		*r = *saved
		return nil
	}
	return fmt.Errorf("cortex: run %s: %w", r.ID, cortex.ErrRunLeaseLost)
}

// settleLease is called by every writer that takes a run out of running
// (a terminal write, a pause) before it writes. For a leased run it stops
// the heartbeat, so no renewal races the write, and checks the run is
// still this pass's to write: an interrupted run is left or handed off
// instead, and a run whose lease another worker took is left alone.
// leave is false when the caller should go on and write; otherwise err is
// what the caller returns in place of its write.
func (e *Engine) settleLease(ctx context.Context, r *run.Run) (leave bool, err error) {
	l := leaseFor(ctx, r.ID)
	if l == nil {
		return false, nil
	}
	l.stop()
	if interruption(ctx, r.ID) != nil {
		return true, e.leaveRun(ctx, r, nil)
	}
	// One last renewal is the fence: a worker whose lease was taken while
	// it was stalled learns it here, before it writes over the new owner.
	// The run's context is left as it is, so a stream can still tell its
	// consumer how the run ended.
	if renewErr := l.renew(); errors.Is(renewErr, cortex.ErrRunLeaseLost) {
		return true, e.loseRun(l, r)
	}
	return false, nil
}

// leaseRegistry is the leased runs this engine is executing.
type leaseRegistry struct {
	mu   sync.Mutex
	held map[*runLease]struct{}
}

func (g *leaseRegistry) add(l *runLease) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held == nil {
		g.held = make(map[*runLease]struct{})
	}
	g.held[l] = struct{}{}
}

func (g *leaseRegistry) remove(l *runLease) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.held, l)
}

// interruptAll cancels every leased run still executing with cause, which
// their loops read as an instruction to hand off.
func (g *leaseRegistry) interruptAll(cause error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for l := range g.held {
		l.interrupt(cause)
	}
}
