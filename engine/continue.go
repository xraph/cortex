package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/telemetry"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

// resumeLabelContinue is the resume source a ContinueRun span reports. The
// run's own source is only known once its row is read.
const resumeLabelContinue = "continue"

// DecideCheckpoint records d and, for an approval, marks the run ready. It
// never calls the model.
//
// It is the half of ResolveCheckpoint a request handler can afford: it
// returns once the decision is written, and the run waits in
// run.StateReady for ContinueRun, which a host usually calls from a
// background job on any replica after plugin.RunReady fires.
//
// For a checkpoint the loop opened, an approval runs every check a
// synchronous resume would run before anything moves, then marks the run
// ready with an instruction to execute each pending call, records the
// decision, and fires CheckpointResolved and RunReady. The run is marked
// ready before the decision is recorded, so the ready state never points
// at a decision nobody can see; if recording fails, repeating the same
// decision finishes the job and the run continues once. A suspension past
// its deadline is refused with cortex.ErrSuspensionExpired and the run is
// left for the expiry sweep.
//
// A rejection fails the run with the decider's reason, as it always has.
// With WithRejectionsReturnToModel it is handled like an approval instead:
// each pending call is answered "rejected by <DecidedBy>: <Reason>", and
// the run is marked ready to carry that back to the model.
//
// A checkpoint a host wrote itself is only recorded. A checkpoint that is
// no longer pending is cortex.ErrInvalidState. ErrInvalidState also means
// "already being acted on": a decider who repeats its own approval in the
// moment between a job claiming the run and that job recording the
// decision is refused with it too, though the approval stands and the run
// is carrying it out. Read the checkpoint to see the record. A store
// without run.ContinuationStore returns cortex.ErrContinuationUnavailable;
// use ResolveCheckpoint there.
func (e *Engine) DecideCheckpoint(ctx context.Context, checkpointID id.CheckpointID, d checkpoint.Decision) error {
	_, err := e.decideCheckpoint(ctx, checkpointID, d)
	return err
}

// decideCheckpoint is DecideCheckpoint, returning the run it made ready,
// or nil when the decision left nothing to continue.
func (e *Engine) decideCheckpoint(ctx context.Context, cpID id.CheckpointID, d checkpoint.Decision) (*run.Run, error) {
	if err := e.requireContinuation(); err != nil {
		return nil, err
	}
	cp, d, err := e.openDecision(ctx, cpID, d)
	if err != nil {
		return nil, err
	}
	if !openedASuspendedRun(cp) {
		return nil, e.recordDecision(ctx, cp, cp.Scope, d)
	}
	susp, err := e.boundSuspension(ctx, cp, true)
	if err != nil {
		return nil, err
	}
	if !d.Approved && !e.rejectionsToModel {
		if rejectErr := e.failRejected(ctx, cp, susp, d); rejectErr != nil {
			if errors.Is(rejectErr, cortex.ErrNotSuspended) && e.runMovedOn(ctx, cp) {
				return nil, fmt.Errorf("resolve checkpoint %s: already decided: %w", cp.ID, cortex.ErrInvalidState)
			}
			return nil, rejectErr
		}
		return nil, e.recordDecision(ctx, cp, cp.Scope, d)
	}

	source := run.ReadyApproval
	if !d.Approved {
		source = run.ReadyRejection
	}
	in := decisionInput(susp.Pending, d)
	if _, previewErr := e.previewResume(ctx, cp.RunID, in, resumeSourceApproval, susp, nil); previewErr != nil {
		return nil, previewErr
	}
	readiness := run.Readiness{Source: source, CheckpointID: cp.ID, SuspensionID: susp.ID, DecidedBy: d.DecidedBy, DecidedByPrincipal: d.DecidedByPrincipal}
	ready, err := e.markReady(ctx, cp.RunID, readiness, in)
	// marked is whether this call's own MarkReady moved the run. A retry
	// that finds the run already ready on this decision did not.
	marked := err == nil
	if errors.Is(err, cortex.ErrNotSuspended) {
		ready, err = e.retriedDecision(ctx, cp, source, err)
	}
	if err != nil {
		return nil, err
	}
	// A decider that lost a race to another decision, or to a cancel that
	// closed the checkpoint, is refused here with ErrInvalidState and
	// fires no hook: the first record stands.
	if err := e.recordDecision(ctx, cp, ready.Scope, d); err != nil {
		if !errors.Is(err, cortex.ErrInvalidState) || !e.recordedAs(ctx, cp, d) {
			return nil, err
		}
		// Somebody recorded this very decision between MarkReady and this
		// write: a job that claimed the run, or the same decider deciding
		// twice at once. The decision stands, so it is a success. RunReady
		// goes out only from the call whose MarkReady moved the run, and
		// only if nobody has claimed the run yet; a claimed run needs no
		// job, and a second notice would send a second job after it.
		current, stillReady := e.readyForCheckpoint(ctx, cp)
		if !stillReady {
			return nil, nil
		}
		if marked {
			e.emitRunReady(ctx, current)
		}
		return current, nil
	}
	e.emitRunReady(ctx, ready)
	return ready, nil
}

// boundSuspension returns the suspension a loop checkpoint was opened for,
// which is the only pause a decision on it may answer.
//
// A run with no suspension is refused with cortex.ErrInvalidState and the
// checkpoint is left alone: that is also what the run looks like while
// another decider is between acting and recording, and the record is
// theirs to write.
//
// A run waiting on a different suspension has left this checkpoint's
// pause behind, and approving it must not approve what the run is waiting
// on now, which nobody has decided. The decision is refused with
// cortex.ErrInvalidState. With closeStale, the ContinuationStore path, the
// checkpoint is also closed as superseded: there a decision is recorded
// before the run acts, so a checkpoint still pending on a run that moved
// on is one whose record was lost. The legacy path acts before recording
// and never closes one, since its own decider may still be writing.
func (e *Engine) boundSuspension(ctx context.Context, cp *checkpoint.Checkpoint, closeStale bool) (*suspension.Suspension, error) {
	susp, err := e.store.GetSuspension(ctx, cp.RunID)
	if errors.Is(err, cortex.ErrNotSuspended) {
		return nil, fmt.Errorf("resolve checkpoint %s: the run is not waiting on it: %w", cp.ID, cortex.ErrInvalidState)
	}
	if err != nil {
		return nil, fmt.Errorf("load suspension for checkpoint %s: %w", cp.ID, err)
	}
	if opened, ok := cp.Metadata[checkpointSuspensionKey].(string); ok && opened == susp.ID.String() {
		return susp, nil
	}
	if closeStale {
		e.closeCheckpoint(ctx, cp, supersededReason)
	}
	return nil, fmt.Errorf("resolve checkpoint %s: the run is no longer waiting on it: %w", cp.ID, cortex.ErrInvalidState)
}

// closeCheckpoint resolves one checkpoint on the engine's behalf, the way
// closePendingCheckpoints does for a run that ended. It is best effort: a
// checkpoint somebody decided in the meantime keeps their decision.
func (e *Engine) closeCheckpoint(ctx context.Context, cp *checkpoint.Checkpoint, reason string) {
	decision := checkpoint.Decision{DecidedBy: checkpointDecidedByEngine, Reason: reason, DecidedAt: time.Now().UTC()}
	if err := e.store.Resolve(cortex.WithScope(ctx, cp.Scope), cp.ID, decision); err != nil && !errors.Is(err, cortex.ErrInvalidState) {
		e.logError("close a checkpoint", err, log.String("checkpoint_id", cp.ID.String()), log.String("run_id", cp.RunID.String()))
	}
}

// openDecision reads the checkpoint a decision is about and stamps the
// decision with its time and its decider.
//
// The decider is recorded from the context, never from the caller's
// decision, and before anything moves: a decision whose decider cannot be
// recorded is not acted on. The run itself continues under its own
// principal, which the continuation restores.
func (e *Engine) openDecision(ctx context.Context, cpID id.CheckpointID, d checkpoint.Decision) (*checkpoint.Checkpoint, checkpoint.Decision, error) {
	if d.DecidedAt.IsZero() {
		d.DecidedAt = time.Now().UTC()
	}
	// Read before write: the row is what says which run this decision is
	// about, and Resolve does not hand it back.
	cp, err := e.store.GetCheckpoint(ctx, cpID)
	if err != nil {
		return nil, d, fmt.Errorf("load checkpoint: %w", err)
	}
	if cp.State != checkpointStatePending {
		// A decided checkpoint is not re-decidable. Without this, a
		// rejection could be followed by an approval that tries to
		// resume a run the rejection already failed, and the caller
		// would read the claim's refusal instead of being told the
		// decision was already made.
		return nil, d, fmt.Errorf("resolve checkpoint %s: already %s: %w", cpID, cp.State, cortex.ErrInvalidState)
	}
	decidedBy, err := e.encodePrincipal(ctx)
	if err != nil {
		return nil, d, fmt.Errorf("resolve checkpoint %s: %w", cpID, err)
	}
	d.DecidedByPrincipal = decidedBy
	return cp, d, nil
}

// recordDecision writes the decision on the checkpoint and tells
// CheckpointResolved subscribers, with ctx on scope, the run's own. A
// checkpoint that is no longer pending keeps its first decision, and this
// one is refused with cortex.ErrInvalidState.
func (e *Engine) recordDecision(ctx context.Context, cp *checkpoint.Checkpoint, scope cortex.Scope, d checkpoint.Decision) error {
	if err := e.store.Resolve(ctx, cp.ID, d); err != nil {
		return fmt.Errorf("record checkpoint %s as resolved: %w", cp.ID, err)
	}
	e.extensions.EmitCheckpointResolved(cortex.WithScope(ctx, scope), cp.ID, decisionLabel(d))
	return nil
}

// retriedDecision is a repeated decision whose first attempt marked the
// run ready and then lost the write recording it. The run is ready for
// this checkpoint and this kind of decision, and the checkpoint is still
// pending, so the decision is recorded now and the run continues once.
//
// Anything else is the refusal MarkReady gave. A checkpoint decided in the
// meantime is cortex.ErrInvalidState, as it would have been had this call
// read it a moment later.
func (e *Engine) retriedDecision(ctx context.Context, cp *checkpoint.Checkpoint, source run.ReadySource, refused error) (*run.Run, error) {
	current, err := e.store.GetCheckpoint(ctx, cp.ID)
	if err != nil {
		return nil, fmt.Errorf("reload checkpoint %s: %w", cp.ID, err)
	}
	if current.State != checkpointStatePending {
		return nil, fmt.Errorf("resolve checkpoint %s: already %s: %w", cp.ID, current.State, cortex.ErrInvalidState)
	}
	r, err := e.store.GetRun(ctx, cp.RunID)
	if err != nil {
		return nil, fmt.Errorf("load run %s for a repeated decision: %w", cp.RunID, err)
	}
	if r.State != run.StateReady || r.Ready == nil || r.Ready.CheckpointID != cp.ID {
		if r.State != run.StatePaused {
			// A decision moved the run first (a rejection that failed it,
			// or an approval a job is already carrying out, which may be
			// this decider's own) and has not been recorded yet. It
			// stands, and the run is already acting on it.
			return nil, fmt.Errorf("resolve checkpoint %s: already decided and being acted on: %w", cp.ID, cortex.ErrInvalidState)
		}
		return nil, refused
	}
	if r.Ready.Source != source {
		// The other decision moved the run and has not been recorded yet.
		// It stands; this one is too late, exactly as if it had read the
		// checkpoint after the record.
		return nil, fmt.Errorf("resolve checkpoint %s: already decided %s: %w", cp.ID, r.Ready.Source, cortex.ErrInvalidState)
	}
	return r, nil
}

// readyForCheckpoint returns the run behind cp when it is ready on a
// decision about cp.
func (e *Engine) readyForCheckpoint(ctx context.Context, cp *checkpoint.Checkpoint) (*run.Run, bool) {
	r, err := e.store.GetRun(ctx, cp.RunID)
	if err != nil || r.State != run.StateReady || r.Ready == nil || r.Ready.CheckpointID != cp.ID {
		return nil, false
	}
	return r, true
}

// runMovedOn reports whether the run behind cp has left paused, which is
// what a decision losing to another one finds.
func (e *Engine) runMovedOn(ctx context.Context, cp *checkpoint.Checkpoint) bool {
	r, err := e.store.GetRun(ctx, cp.RunID)
	return err == nil && r.State != run.StatePaused
}

// recordedAs reports whether cp already carries d: the same outcome by the
// same decider. That is a job recording this decision on its own when it
// claimed the run first.
func (e *Engine) recordedAs(ctx context.Context, cp *checkpoint.Checkpoint, d checkpoint.Decision) bool {
	current, err := e.store.GetCheckpoint(ctx, cp.ID)
	if err != nil || current.Decision == nil {
		return false
	}
	got := current.Decision
	return got.Approved == d.Approved && got.DecidedBy == d.DecidedBy && bytes.Equal(got.DecidedByPrincipal, d.DecidedByPrincipal)
}

// decisionInput is what a decision hands back for each pending call. An
// approval asks the engine to execute each one, since the calls were
// stopped before dispatch. A rejection answers each one with an error the
// model reads.
func decisionInput(pending []suspension.PendingCall, d checkpoint.Decision) ResumeInput {
	in := ResumeInput{ToolResults: make([]ToolResult, 0, len(pending)), rejected: !d.Approved}
	for _, p := range pending {
		if d.Approved {
			in.ToolResults = append(in.ToolResults, ToolResult{ToolCallID: p.ID, Execute: true})
		} else {
			in.ToolResults = append(in.ToolResults, ToolResult{ToolCallID: p.ID, Error: rejectionMessage(d)})
		}
	}
	return in
}

// rejectionMessage is the error a rejected call returns to the model.
func rejectionMessage(d checkpoint.Decision) string {
	by, reason := d.DecidedBy, d.Reason
	if by == "" {
		by = "a reviewer"
	}
	if reason == "" {
		reason = "no reason given"
	}
	return fmt.Sprintf("rejected by %s: %s", by, reason)
}

// ProvideResults answers an external-tool pause and marks the run ready.
// It never calls the model.
//
// The results are checked against the pause before anything is written:
// one per pending call, or cortex.ErrResultsMismatch. A run paused for
// approval is cortex.ErrRequiresApproval, and one waiting on another
// agent is ErrNotAgentReplyResumable, exactly as Resume refuses them.
// Every refusal leaves the run paused. Once the run is ready, RunReady
// fires and ContinueRun continues it.
//
// A store without run.ContinuationStore returns
// cortex.ErrContinuationUnavailable; use Resume there.
func (e *Engine) ProvideResults(ctx context.Context, runID id.AgentRunID, in ResumeInput) error {
	if err := e.requireContinuation(); err != nil {
		return err
	}
	if _, err := cortex.RequireScope(ctx); err != nil {
		return err
	}
	in = ResumeInput{ToolResults: in.ToolResults}
	susp, err := e.store.GetSuspension(ctx, runID)
	if err != nil {
		return fmt.Errorf("load suspension to answer run %s: %w", runID, err)
	}
	if _, previewErr := e.previewResume(ctx, runID, in, resumeSourcePublic, susp, nil); previewErr != nil {
		return previewErr
	}
	ready, err := e.markReady(ctx, runID, run.Readiness{Source: run.ReadyExternalResults, SuspensionID: susp.ID}, in)
	if err != nil {
		return err
	}
	e.emitRunReady(ctx, ready)
	return nil
}

// markReady stores in with the run and moves it to ready. readiness names
// the pause in answers, which MarkReady checks is still the run's.
func (e *Engine) markReady(ctx context.Context, runID id.AgentRunID, readiness run.Readiness, in ResumeInput) (*run.Run, error) {
	results, err := json.Marshal(in.ToolResults)
	if err != nil {
		return nil, fmt.Errorf("encode results for run %s: %w", runID, err)
	}
	readiness.Results, readiness.At = results, time.Now().UTC()
	ready, err := e.continuation.MarkReady(ctx, runID, readiness)
	if err != nil {
		return nil, fmt.Errorf("mark run %s ready: %w", runID, err)
	}
	return ready, nil
}

// emitRunReady tells RunReady subscribers, under the run's own scope.
func (e *Engine) emitRunReady(ctx context.Context, r *run.Run) {
	if r == nil || r.Ready == nil {
		return
	}
	notice := run.ReadyNotice{Source: r.Ready.Source, CheckpointID: r.Ready.CheckpointID, At: r.Ready.At}
	runCtx := cortex.WithScope(ctx, r.Scope)
	e.extensions.EmitRunReady(runCtx, r.AgentID, r.ID, notice)
	e.markNotified(runCtx, r.ID, time.Now().UTC())
}

// markNotified records that the host was just told about a waiting run,
// so the sweeper repeats the notice only once the run has waited a whole
// WithReadyRenotify period since. A run claimed in the meantime is no
// longer waiting, and that refusal is not an error.
func (e *Engine) markNotified(ctx context.Context, runID id.AgentRunID, at time.Time) {
	if e.continuation == nil {
		return
	}
	if err := e.continuation.MarkNotified(ctx, runID, at); err != nil && !errors.Is(err, cortex.ErrRunNotContinuable) {
		e.logWarn("record a run notice", err, log.String("run_id", runID.String()))
	}
}

// requireContinuation is the capability check every host-driven call
// makes first.
func (e *Engine) requireContinuation() error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	if e.continuation == nil {
		return cortex.ErrContinuationUnavailable
	}
	return nil
}

// ContinueRun runs a ready or reclaimable run in the caller's goroutine
// under the run's original principal.
//
// It is the job half of DecideCheckpoint and ProvideResults. It reads the
// run, runs the pre-claim checks a resume runs, restores the run's
// principal, claims the run, and then executes the recorded decision or
// results and the rest of the loop, exactly as Resume would. ctx must
// carry the run's own scope, which is the scope on the RunReady hook's
// ctx.
//
// Anything refused before the claim for now leaves the run ready for the
// next attempt, including a principal that cannot be decoded right now. A
// principal that is revoked fails the run, and so does a check no later
// attempt can pass (its stored answer does not fit the pause, say), which
// also closes its checkpoint.
//
// A reclaimable run, or a running run whose lease has lapsed (which this
// call marks reclaimable itself), continues from its last saved step: see
// claimReclaimed. It fails with cortex.ErrReclaimLimit once reclaimed more
// often than WithReclaimLimit, with cortex.ErrRunNotRecoverable when it
// left nothing to continue from, and stops with an error matching
// cortex.ErrEffectUncertain when a call it had handed to a tool that is
// not retry-safe has no recorded outcome. A run claimed for a cancel or
// an expiry whose write was lost is ended that way instead, and comes back
// with cortex.ErrRunAlreadyDone or the expiry's error.
//
// A run in any other state is not continued. ErrRunNotContinuable covers
// every such refusal, and the error also matches the specific case:
// cortex.ErrRunLeased for a running run under a live lease, and
// cortex.ErrRunAlreadyDone, returned with the run, for one that has ended.
// Two callers continuing the same run: one runs it, the other is refused.
//
// A store without run.ContinuationStore returns
// cortex.ErrContinuationUnavailable.
func (e *Engine) ContinueRun(ctx context.Context, runID id.AgentRunID) (*run.Run, error) {
	if err := e.requireContinuation(); err != nil {
		return nil, err
	}
	var ended *run.Run
	r, err := e.traceResume(ctx, runID, resumeLabelContinue, func(tc context.Context) (*run.Run, error) {
		runCtx, rz, done, claimErr := e.claimContinuation(tc, runID)
		if claimErr != nil {
			ended = done
			return nil, claimErr
		}
		if rz.paused != nil {
			return rz.run, nil
		}
		return e.continueReAct(runCtx, rz.ag, rz.cfg, rz.run, rz.state, rz.startedAt)
	})
	if r == nil && ended != nil {
		return ended, err
	}
	return r, err
}

// ContinueRunStream is ContinueRun over the streaming loop. It closes
// events when execution completes, like ResumeStream, and the first event
// is run_started with resumed set.
func (e *Engine) ContinueRunStream(ctx context.Context, runID id.AgentRunID, events chan<- StreamEvent) (err error) {
	if capabilityErr := e.requireContinuation(); capabilityErr != nil {
		close(events)
		return capabilityErr
	}
	ctx, done, err := e.admit(ctx)
	if err != nil {
		close(events)
		return err
	}
	ctx, rt := e.startRun(ctx, modeStream, true,
		attribute.String(attrRunID, runID.String()),
		attribute.String(attrRunResumeSource, resumeLabelContinue))
	defer func() {
		if p := recover(); p != nil {
			done()
			rt.end(nil, telemetry.ErrPanic)
			panic(p)
		}
		if err != nil {
			done()
			rt.end(nil, err)
		}
	}()
	ctx, rz, _, err := e.claimContinuation(ctx, runID)
	if err != nil {
		close(events)
		return err
	}
	go e.streamResumed(ctx, rz, rt, events, done)
	return nil
}

// claimContinuation takes a ready run for this call and rebuilds what the
// loop needs to continue it. A run that has ended comes back as done, with
// the error.
func (e *Engine) claimContinuation(ctx context.Context, runID id.AgentRunID) (context.Context, *resumption, *run.Run, error) {
	if _, err := cortex.RequireScope(ctx); err != nil {
		return nil, nil, nil, err
	}
	current, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load run to continue: %w", err)
	}
	switch {
	case current.State == run.StateReady:
	case current.State == run.StateReclaimable:
		return e.claimReclaimed(ctx, current)
	case current.State == run.StateRunning && leaseLapsed(current, time.Now().UTC()):
		// The worker's lease lapsed and no sweep has taken the run yet: take
		// it here, so a host retrying the job does not wait for the sweeper.
		reclaimed, reclaimErr := e.reclaimInline(ctx, current)
		if reclaimErr != nil {
			return nil, nil, nil, reclaimErr
		}
		return e.claimReclaimed(ctx, reclaimed)
	case current.State == run.StateRunning:
		return nil, nil, nil, fmt.Errorf("cortex: continue run %s: run is running: %w: %w", runID, cortex.ErrRunNotContinuable, cortex.ErrRunLeased)
	case isTerminalRunState(current.State):
		return nil, nil, current, fmt.Errorf("cortex: continue run %s: run is %s: %w: %w", runID, current.State, cortex.ErrRunNotContinuable, cortex.ErrRunAlreadyDone)
	default:
		// Paused or created.
		return nil, nil, nil, fmt.Errorf("cortex: continue run %s: run is %s: %w", runID, current.State, cortex.ErrRunNotContinuable)
	}

	// The run's principal replaces the caller's first, as in
	// claimForResume: a decode that fails for now leaves the run ready, and
	// a revoked principal is held until the claim makes the run this
	// call's to fail. The caller is a job acting for nobody in particular,
	// so the checks below ask about the run's principal, the one the run
	// will continue under.
	ctx, revoked, err := e.restorePrincipal(ctx, current)
	if err != nil {
		return nil, nil, nil, err
	}

	// A run that is about to be failed for its principal or its stored
	// input has nothing to check. Any other run is checked as DecideCheckpoint
	// and ProvideResults checked it, against this engine's configuration
	// now, and a refusal leaves it ready.
	in, source, inputErr := readyInput(current.Ready)
	snapshot := current
	var first *suspension.Progress
	if revoked == nil && inputErr == nil {
		// A suspension that cannot be read now is a refusal before the
		// claim like any other: the run stays ready for the next attempt.
		susp, suspErr := e.store.GetSuspension(ctx, runID)
		if suspErr != nil {
			return nil, nil, nil, fmt.Errorf("load suspension to continue run %s: %w", runID, suspErr)
		}
		// The first snapshot carries every answer the run is ready with,
		// so a worker that dies after the claim leaves them for the next.
		first = claimSnapshot(susp, answeredCalls(susp.Pending, in))
		snapshot, err = e.previewResume(ctx, runID, in, source, susp, nil)
		if err != nil && !permanentRefusal(err) {
			return nil, nil, nil, err
		}
		if err != nil {
			// No later attempt can pass this check, so leaving the run
			// ready would leave it ready for good, re-announced forever.
			// It is claimed below and failed with the refusal instead.
			inputErr = fmt.Errorf("cortex: continue run %s: %w", runID, err)
			snapshot = current
		}
	}

	// A run this engine leases is claimed under this worker's lease, which
	// the heartbeat then renews. Runs under required verification or a
	// task claim, and every run when leases are off, are claimed with no
	// lease and never heartbeated: coordination owns their recovery.
	if inputErr != nil || revoked != nil {
		first = nil
	}
	claimed, written, err := e.claimContinuable(ctx, current, first)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("claim run %s to continue: %w", runID, err)
	}
	ctx, held := e.adoptLease(ctx, claimed, first)
	if held && !written && first != nil {
		if saveErr := e.saveProgress(ctx, runID, first); saveErr != nil {
			return nil, nil, nil, saveErr
		}
	}

	// From here the run is running and belongs to this call, so every exit
	// either hands the loop a runnable state or fails the run.
	runCtx := cortex.WithScope(ctx, claimed.Scope)
	e.recordLostDecision(runCtx, current.Ready)
	susp, err := e.store.GetSuspension(runCtx, runID)
	if err != nil {
		return nil, nil, nil, e.failResume(runCtx, claimed, claimed.AgentID, fmt.Errorf("load suspension of a claimed run: %w", err))
	}
	susp.ClaimedRun = claimed
	if held && first == nil {
		e.saveBoundary(runCtx, runID, pausedCont(susp.Cont))
	}
	if revoked != nil {
		return nil, nil, nil, e.failRevokedResume(ctx, snapshot, susp, revoked)
	}
	if inputErr != nil {
		failErr := e.failResume(runCtx, claimed, claimed.AgentID, inputErr)
		if delErr := e.store.DeleteSuspension(runCtx, runID); delErr != nil && !errors.Is(delErr, cortex.ErrNotSuspended) {
			e.logError("delete the pause of a run that could not continue", delErr, log.String("run_id", runID.String()))
		}
		e.closePendingCheckpoints(runCtx, runID, "the run could not be continued on this decision, so it was failed and there is nothing left to decide")
		return nil, nil, nil, failErr
	}
	runCtx, rz, err := e.rehydrateResume(ctx, in, source, snapshot, susp)
	return runCtx, rz, nil, err
}

// claimContinuable claims a ready or reclaimable run for this call. A run
// this engine leases is claimed under this worker's lease, which the
// heartbeat then renews, and first is written in the same transaction when
// the store can (written reports it). Runs under required verification or
// a task claim, and every run when leases are off, are claimed with no
// lease and never heartbeated: coordination owns their recovery.
func (e *Engine) claimContinuable(ctx context.Context, current *run.Run, first *suspension.Progress) (claimed *run.Run, written bool, err error) {
	if !e.leasesRun(current) {
		claimed, err = e.continuation.ClaimContinuation(ctx, current.ID, run.Lease{})
		return claimed, false, err
	}
	lease := e.newLease()
	if store, ok := e.leasedClaimsFor(current); ok {
		if first != nil {
			first.Owner = lease.Owner
		}
		claimed, err = store.ClaimContinuationLeased(ctx, current.ID, lease, first)
		return claimed, first != nil, err
	}
	claimed, err = e.continuation.ClaimContinuation(ctx, current.ID, lease)
	return claimed, false, err
}

// permanentRefusal reports whether a check a ready run failed before its
// claim will fail the same way on every later attempt: the stored answer
// or the stored continuation is wrong, not the moment. Anything else (a
// store that could not be read, a policy this engine cannot apply right
// now) leaves the run ready for the next attempt.
func permanentRefusal(err error) bool {
	for _, sentinel := range []error{cortex.ErrResultsMismatch, cortex.ErrInvalidContinuation, cortex.ErrInvalidPrepared, cortex.ErrRequiresApproval, ErrNotAgentReplyResumable} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return false
}

// recordLostDecision records the decision a ready run carries on its
// checkpoint, when the checkpoint is still pending: DecideCheckpoint lost
// that write, or has not made it yet. In the second case the decider's own
// write then finds this record and treats it as its own. The run is being
// carried out on the decision, so the queue must not go on asking for it.
// The decider is the one the readiness kept; a readiness without one is
// recorded as the engine's, with a reason saying the original record was
// lost.
//
// Between the claim and this record a decider repeating its decision is
// told cortex.ErrInvalidState, which here means the decision is already
// being acted on, not that it was refused.
func (e *Engine) recordLostDecision(ctx context.Context, ready *run.Readiness) {
	if ready == nil || ready.CheckpointID.IsNil() {
		return
	}
	cp, err := e.store.GetCheckpoint(ctx, ready.CheckpointID)
	if err != nil {
		e.logError("read the checkpoint of a continued run", err, log.String("checkpoint_id", ready.CheckpointID.String()))
		return
	}
	if cp.State != checkpointStatePending {
		return
	}
	d := checkpoint.Decision{
		Approved:           ready.Source == run.ReadyApproval,
		DecidedBy:          ready.DecidedBy,
		DecidedByPrincipal: append([]byte(nil), ready.DecidedByPrincipal...),
		Reason:             "recorded when the run continued",
		DecidedAt:          ready.At,
	}
	if d.DecidedBy == "" && len(d.DecidedByPrincipal) == 0 {
		d.DecidedBy = checkpointDecidedByEngine
		d.Reason = "the run was continued on this decision, but who made it was not recorded"
	}
	if err := e.store.Resolve(ctx, cp.ID, d); err != nil {
		if !errors.Is(err, cortex.ErrInvalidState) {
			e.logError("record the lost decision of a continued run", err, log.String("checkpoint_id", cp.ID.String()))
		}
		return
	}
	e.extensions.EmitCheckpointResolved(ctx, cp.ID, decisionLabel(d))
}

// readyInput decodes what a ready run continues with, and the authority it
// arrives under. A rejection going back to the model continues with the
// approval's authority, since a checkpoint decision is what produced it.
func readyInput(ready *run.Readiness) (ResumeInput, resumeSource, error) {
	var in ResumeInput
	if ready == nil {
		return in, resumeSourcePublic, fmt.Errorf("ready run carries nothing to continue with: %w", cortex.ErrInvalidContinuation)
	}
	if err := json.Unmarshal(ready.Results, &in.ToolResults); err != nil {
		return in, resumeSourcePublic, fmt.Errorf("decode ready results: %w: %w", cortex.ErrInvalidContinuation, err)
	}
	switch ready.Source {
	case run.ReadyApproval:
		return in, resumeSourceApproval, nil
	case run.ReadyRejection:
		for _, res := range in.ToolResults {
			if res.Execute || res.Error == "" {
				return in, resumeSourcePublic, fmt.Errorf("a rejection asks for a call to run: %w", cortex.ErrInvalidContinuation)
			}
		}
		in.rejected = true
		return in, resumeSourceApproval, nil
	case run.ReadyExternalResults:
		return in, resumeSourcePublic, nil
	default:
		return in, resumeSourcePublic, fmt.Errorf("unknown ready source %q: %w", ready.Source, cortex.ErrInvalidContinuation)
	}
}
