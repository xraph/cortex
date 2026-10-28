package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"

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
// no longer pending is cortex.ErrInvalidState. A store without
// run.ContinuationStore returns cortex.ErrContinuationUnavailable; use
// ResolveCheckpoint there.
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
		return nil, e.recordDecision(ctx, cp, d)
	}
	if !d.Approved && !e.rejectionsToModel {
		if rejectErr := e.failRejected(ctx, cp, d); rejectErr != nil {
			return nil, rejectErr
		}
		return nil, e.recordDecision(ctx, cp, d)
	}

	source := run.ReadyApproval
	if !d.Approved {
		source = run.ReadyRejection
	}
	susp, err := e.store.GetSuspension(ctx, cp.RunID)
	if err != nil {
		return nil, fmt.Errorf("load suspension for checkpoint %s: %w", cpID, err)
	}
	in := decisionInput(susp.Pending, d)
	if _, previewErr := e.previewResume(ctx, cp.RunID, in, resumeSourceApproval, susp, nil); previewErr != nil {
		return nil, previewErr
	}
	ready, err := e.markReady(ctx, cp.RunID, source, in, cp.ID)
	if errors.Is(err, cortex.ErrNotSuspended) {
		ready, err = e.retriedDecision(ctx, cp, source, err)
	}
	if err != nil {
		return nil, err
	}
	if err := e.recordDecision(ctx, cp, d); err != nil {
		return nil, err
	}
	e.emitRunReady(ctx, ready)
	return ready, nil
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
// CheckpointResolved subscribers.
func (e *Engine) recordDecision(ctx context.Context, cp *checkpoint.Checkpoint, d checkpoint.Decision) error {
	if err := e.store.Resolve(ctx, cp.ID, d); err != nil {
		return fmt.Errorf("record checkpoint %s as resolved: %w", cp.ID, err)
	}
	e.extensions.EmitCheckpointResolved(ctx, cp.ID, decisionLabel(d))
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
	if r.State != run.StateReady || r.Ready == nil || r.Ready.CheckpointID != cp.ID || r.Ready.Source != source {
		return nil, refused
	}
	return r, nil
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
	ready, err := e.markReady(ctx, runID, run.ReadyExternalResults, in, id.CheckpointID{})
	if err != nil {
		return err
	}
	e.emitRunReady(ctx, ready)
	return nil
}

// markReady stores in with the run and moves it to ready.
func (e *Engine) markReady(ctx context.Context, runID id.AgentRunID, source run.ReadySource, in ResumeInput, cpID id.CheckpointID) (*run.Run, error) {
	results, err := json.Marshal(in.ToolResults)
	if err != nil {
		return nil, fmt.Errorf("encode results for run %s: %w", runID, err)
	}
	ready, err := e.continuation.MarkReady(ctx, runID, run.Readiness{Source: source, Results: results, CheckpointID: cpID, At: time.Now().UTC()})
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
	e.extensions.EmitRunReady(cortex.WithScope(ctx, r.Scope), r.AgentID, r.ID, notice)
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
// Anything refused before the claim leaves the run ready for the next
// attempt, including a principal that cannot be decoded right now. A
// principal that is revoked fails the run.
//
// A run in any other state is not continued. ErrRunNotContinuable covers
// every such refusal, and the error also matches the specific case:
// cortex.ErrRunLeased for a running run, and cortex.ErrRunAlreadyDone,
// returned with the run, for one that has ended. Two callers continuing
// the same run: one runs it, the other is refused.
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
	case current.State == run.StateRunning:
		return nil, nil, nil, fmt.Errorf("cortex: continue run %s: run is running: %w: %w", runID, cortex.ErrRunNotContinuable, cortex.ErrRunLeased)
	case isTerminalRunState(current.State):
		return nil, nil, current, fmt.Errorf("cortex: continue run %s: run is %s: %w: %w", runID, current.State, cortex.ErrRunNotContinuable, cortex.ErrRunAlreadyDone)
	default:
		// Paused, created, and reclaimable until reclaiming lands.
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
	if revoked == nil && inputErr == nil {
		susp, suspErr := e.store.GetSuspension(ctx, runID)
		snapshot, err = e.previewResume(ctx, runID, in, source, susp, suspErr)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	// No lease yet: the run is held by this call alone until leasing
	// lands. Runs under required verification or a task claim never get
	// one, since coordination owns their recovery.
	claimed, err := e.continuation.ClaimContinuation(ctx, runID, run.Lease{})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("claim run %s to continue: %w", runID, err)
	}

	// From here the run is running and belongs to this call, so every exit
	// either hands the loop a runnable state or fails the run.
	runCtx := cortex.WithScope(ctx, claimed.Scope)
	susp, err := e.store.GetSuspension(runCtx, runID)
	if err != nil {
		return nil, nil, nil, e.failResume(runCtx, claimed, claimed.AgentID, fmt.Errorf("load suspension of a claimed run: %w", err))
	}
	susp.ClaimedRun = claimed
	if revoked != nil {
		return nil, nil, nil, e.failRevokedResume(ctx, snapshot, susp, revoked)
	}
	if inputErr != nil {
		return nil, nil, nil, e.failResume(runCtx, claimed, claimed.AgentID, inputErr)
	}
	runCtx, rz, err := e.rehydrateResume(ctx, in, source, snapshot, susp)
	return runCtx, rz, nil, err
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
