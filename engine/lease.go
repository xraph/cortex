package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
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

	quit    chan struct{}
	done    chan struct{}
	quiesce sync.Once

	mu       sync.Mutex
	expires  time.Time
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
		quit:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	if r.Lease != nil {
		l.expires = r.Lease.ExpiresAt
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

// releaseLease stops renewing the lease ctx holds on runID, if any. The
// loops defer it, so a pass that ends any way at all stops renewing. The
// run's context is left alone, since some callers go on using it after the
// loop; it ends with the admission ticket of the call that ran the run.
func releaseLease(ctx context.Context, runID id.AgentRunID) {
	if l := leaseFor(ctx, runID); l != nil {
		l.stop()
	}
}

// leaseClaimed takes a lease on a run this call just claimed from a pause,
// which the claim left running with no owner. A run this engine does not
// lease, or a hold that fails, continues as before without one.
func (e *Engine) leaseClaimed(ctx context.Context, r *run.Run) context.Context {
	if r == nil || !e.leasesRun(r) {
		return ctx
	}
	lease := e.newLease()
	if err := e.continuation.HoldLease(cortex.WithScope(ctx, r.Scope), r.ID, lease); err != nil {
		e.logWarn("hold the lease of a claimed run; continuing without one", err, log.String("run_id", r.ID.String()))
		return ctx
	}
	r.Lease = &lease
	ctx = e.holdLease(ctx, r)
	return ctx
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
		if err := l.renew(); errors.Is(err, cortex.ErrRunLeaseLost) {
			l.cancel(cortex.ErrRunLeaseLost)
			return
		}
	}
}

// renew extends the lease by the engine's ttl from now.
func (l *runLease) renew() error {
	expires := time.Now().UTC().Add(l.e.leaseTTL)
	ctx, cancel := context.WithTimeout(l.base, leaseWriteTimeout)
	defer cancel()
	err := l.e.continuation.HoldLease(ctx, l.runID, run.Lease{Owner: l.owner, ExpiresAt: expires})
	if err != nil {
		if !errors.Is(err, cortex.ErrRunLeaseLost) {
			l.e.logWarn("renew a run lease", err, log.String("run_id", l.runID.String()))
		}
		return err
	}
	l.mu.Lock()
	l.expires = expires
	l.mu.Unlock()
	return nil
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

// interruption says why a leased run must stop where it is:
// cortex.ErrRunLeaseLost when its lease is gone, cortex.ErrEngineStopping
// when Stop ran out of time. It is nil for any other run, and for a run
// whose context ended for another reason (its deadline, its caller).
func interruption(ctx context.Context, runID id.AgentRunID) error {
	if leaseFor(ctx, runID) == nil || ctx.Err() == nil {
		return nil
	}
	cause := context.Cause(ctx)
	if errors.Is(cause, cortex.ErrRunLeaseLost) || errors.Is(cause, cortex.ErrEngineStopping) {
		return cause
	}
	return nil
}

// handOffDue reports whether a leased run should stop at this boundary:
// the engine is draining, or the run was interrupted.
func (e *Engine) handOffDue(ctx context.Context, runID id.AgentRunID) bool {
	if leaseFor(ctx, runID) == nil {
		return false
	}
	return e.admission.draining() || interruption(ctx, runID) != nil
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
	if errors.Is(interruption(ctx, r.ID), cortex.ErrRunLeaseLost) {
		return e.loseRun(l, r)
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
	e.extensions.EmitRunReclaimable(l.base, r.AgentID, r.ID, run.ReclaimNotice{
		PreviousOwner:  l.owner,
		LeaseExpiredAt: at,
		Reclaims:       r.Reclaims,
		HandOff:        true,
	})
	return cortex.ErrRunInterrupted
}

// loseRun is the end of a pass whose lease went to someone else. Nothing
// is written. A run that has ended in the meantime (an operator cancelled
// it, say) is handed back as it ended, the way finalizeRun adopts a
// terminal state it finds.
func (e *Engine) loseRun(l *runLease, r *run.Run) error {
	readCtx, cancel := context.WithTimeout(l.base, leaseWriteTimeout)
	defer cancel()
	if saved, err := e.store.GetRun(readCtx, r.ID); err == nil && isTerminalRunState(saved.State) {
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
		l.cancel(cause)
	}
}
