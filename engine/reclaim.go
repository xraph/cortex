package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
	"github.com/xraph/cortex/verification"
)

const (
	// defaultReclaimLimit is how often a run may be reclaimed after a
	// lapsed lease before it is failed.
	defaultReclaimLimit = 3
	// defaultReadyRenotify is how long a ready or reclaimable run waits
	// before the sweeper tells the host about it again.
	defaultReadyRenotify = 5 * time.Minute
)

// How a reclaim ended, for cortex_runs_reclaimed_total.
const (
	// reclaimReclaimed: a run whose lease lapsed was marked reclaimable.
	reclaimReclaimed = "reclaimed"
	// reclaimContinued: a reclaimed run was claimed and handed back to the
	// loop, or paused again on a call that needs an answer.
	reclaimContinued = "continued"
	// reclaimUncertain: a call the previous worker handed to its handler
	// has no recorded outcome, so the run stopped.
	reclaimUncertain = "uncertain"
	// reclaimLimit: the run was reclaimed more often than WithReclaimLimit.
	reclaimLimit = "limit"
	// reclaimUnrecoverable: the run left nothing to continue from.
	reclaimUnrecoverable = "unrecoverable"
)

// supersededReason is what a checkpoint closed for a pause its run left
// behind says, on the decide path and in the sweep.
const supersededReason = "superseded: the run is no longer waiting on the pause this checkpoint was opened for"

// idleGraceWithoutLease is idleGrace on an engine that leases nothing.
const idleGraceWithoutLease = 2 * time.Minute

// idleGrace is how long a checkpoint may sit pending on a run that has
// moved past it before the sweep closes it: twice the lease ttl, far
// longer than a decider takes between its write and its record, or
// idleGraceWithoutLease with leases off.
func (e *Engine) idleGrace() time.Duration {
	if e.leaseTTL > 0 {
		return 2 * e.leaseTTL
	}
	return idleGraceWithoutLease
}

// leaseLapsed reports whether r is running under a lease that expired at
// or before now.
func leaseLapsed(r *run.Run, now time.Time) bool {
	return r.State == run.StateRunning && r.Lease != nil && !r.Lease.ExpiresAt.IsZero() && !r.Lease.ExpiresAt.After(now)
}

// reclaimInline takes a running run whose lease lapsed, so that a host
// retrying a job does not have to wait for the sweeper. A lease renewed in
// the meantime, or a run that left running, is cortex.ErrRunLeased.
func (e *Engine) reclaimInline(ctx context.Context, current *run.Run) (*run.Run, error) {
	moved, err := e.continuation.MarkReclaimable(ctx, current.ID, time.Now().UTC(), "")
	if errors.Is(err, cortex.ErrRunLeased) || errors.Is(err, cortex.ErrRunConflict) {
		return nil, fmt.Errorf("cortex: continue run %s: run is running: %w: %w", current.ID, cortex.ErrRunNotContinuable, cortex.ErrRunLeased)
	}
	if err != nil {
		return nil, fmt.Errorf("mark run %s reclaimable: %w", current.ID, err)
	}
	e.instr.reclaim(reclaimReclaimed)
	return moved, nil
}

// claimReclaimed takes a reclaimable run for this call and rebuilds what
// the loop needs to continue it from its last durable step.
//
// In order: what the run continues from is read; a run whose snapshot
// carries the end a cancel or an expiry claimed it for is ended that way;
// a run reclaimed more often than the limit is failed with
// cortex.ErrReclaimLimit; the run's principal is restored as for any
// continuation; then the run is claimed. Everything before the claim
// leaves the run reclaimable when it fails for now. The progress is read
// before the claim because nothing can write it while the run is
// reclaimable: a stale worker's saves need a running run under its own
// lease.
//
// A run with no progress continues from the pause it was last claimed
// from, when that suspension is still there: a worker that claimed the
// run from its pause and died before it took the lease leaves exactly
// that. A run with neither is failed with cortex.ErrRunNotRecoverable.
//
// After the claim, the calls of the step the previous worker was in are
// settled (see replayStep), and the run either goes back to the loop at
// its saved step, pauses again on a call that needs an answer, or stops
// because a call's outcome is unknown. When the run paused again, the
// resumption carries the pause and no loop is to run.
func (e *Engine) claimReclaimed(ctx context.Context, current *run.Run) (context.Context, *resumption, *run.Run, error) {
	runID := current.ID
	saved, stale, err := e.reclaimSource(ctx, runID)
	if err != nil {
		return nil, nil, nil, err
	}
	// A run claimed to be cancelled or expired, whose worker never wrote
	// that end, is ended now the way it was meant to end.
	if saved != nil && saved.Ending != nil {
		ended, endErr := e.finishEnding(ctx, current, saved, stale)
		return nil, nil, ended, endErr
	}
	if current.Reclaims > e.reclaimLimit {
		limitErr := fmt.Errorf("cortex: run %s was reclaimed %d times, more than the limit of %d: %w", runID, current.Reclaims, e.reclaimLimit, cortex.ErrReclaimLimit)
		e.logWarn("fail a run reclaimed too many times", limitErr, log.String("run_id", runID.String()), log.Int("reclaims", current.Reclaims))
		// The pause it was last claimed from, if its row is still there, goes
		// with the run.
		ended, failErr := e.failReclaimed(ctx, current, stale, limitErr)
		e.instr.reclaim(reclaimLimit)
		return nil, nil, ended, failErr
	}

	ctx, revoked, err := e.restorePrincipal(ctx, current)
	if err != nil {
		return nil, nil, nil, err
	}
	if revoked != nil {
		e.logWarn("fail a reclaimed run whose principal was revoked", revoked, log.String("run_id", runID.String()))
		ended, failErr := e.failReclaimed(ctx, current, stale, revoked)
		return nil, nil, ended, failErr
	}
	if saved == nil {
		notRecoverable := fmt.Errorf("cortex: reclaim run %s: %w", runID, cortex.ErrRunNotRecoverable)
		e.logWarn("fail a reclaimed run with nothing to continue from", notRecoverable, log.String("run_id", runID.String()))
		ended, failErr := e.failReclaimed(ctx, current, stale, notRecoverable)
		e.instr.reclaim(reclaimUnrecoverable)
		return nil, nil, ended, failErr
	}

	// The previous worker's snapshot, Started and Done included, is taken
	// over under this worker's lease in the claim itself, so a second stop
	// part way through the replay loses none of it.
	first := &suspension.Progress{RunID: runID, Cont: saved.Cont, Started: slices.Clone(saved.Started), Done: slices.Clone(saved.Done)}
	claimed, written, err := e.claimContinuable(ctx, current, first)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("claim run %s to continue: %w", runID, err)
	}
	ctx, held := e.adoptLease(ctx, claimed, first)
	if held && !written {
		if saveErr := e.saveProgress(ctx, runID, first); saveErr != nil {
			return nil, nil, nil, saveErr
		}
	}

	// From here the run is running and belongs to this call, so every exit
	// either hands the loop a runnable state, pauses the run, or ends it.
	runCtx := cortex.WithScope(ctx, claimed.Scope)
	runTraceFrom(runCtx).bind(claimed, saved.Cont.Config.Model)
	// A directive's run that was handed off owes the asker its answer,
	// whichever worker ends up finishing it.
	owesReply(claimed)
	if stale != nil {
		e.dropStalePause(runCtx, stale)
	}
	loopCtx, rz, ended, err := e.rebuildReclaimed(runCtx, claimed, saved)
	if err != nil {
		if errors.Is(err, cortex.ErrEffectUncertain) {
			e.instr.reclaim(reclaimUncertain)
		}
		return nil, nil, ended, err
	}
	e.instr.reclaim(reclaimContinued)
	return loopCtx, rz, nil, nil
}

// reclaimSource reads what a reclaimable run continues from: its progress
// snapshot, or else the suspension it was last claimed from, as a snapshot
// with nothing started and nothing done. It also returns that suspension
// when the row is still there, since a run that continues must not leave
// it behind. Neither is (nil, nil, nil).
func (e *Engine) reclaimSource(ctx context.Context, runID id.AgentRunID) (*suspension.Progress, *suspension.Suspension, error) {
	stale, err := e.store.GetSuspension(ctx, runID)
	if errors.Is(err, cortex.ErrNotSuspended) {
		stale = nil
	} else if err != nil {
		return nil, nil, fmt.Errorf("load the pause of reclaimed run %s: %w", runID, err)
	}
	if e.progress != nil {
		saved, readErr := e.progress.GetProgress(ctx, runID)
		if readErr == nil {
			return saved, stale, nil
		}
		if !errors.Is(readErr, cortex.ErrProgressNotFound) {
			return nil, nil, fmt.Errorf("load the progress of reclaimed run %s: %w", runID, readErr)
		}
	}
	if stale == nil {
		return nil, nil, nil
	}
	return &suspension.Progress{RunID: runID, Cont: pausedCont(stale.Cont), Started: sentCalls(stale)}, stale, nil
}

// sentCalls are the pending calls of a pause that already left the engine:
// a question to a peer agent, an external call handed to the host. With no
// record of their answers, they count as started, so the replay stops on
// them rather than sending them again. A call waiting on an approval was
// never sent, and is asked about again.
func sentCalls(paused *suspension.Suspension) []suspension.PendingCall {
	if paused.Reason != suspension.ReasonAgentReply && paused.Reason != suspension.ReasonExternalTool {
		return nil
	}
	return slices.Clone(paused.Pending)
}

// finishEnding writes the end a cancel or an expiry claimed the run for
// and did not get to write, the way that path would have: a cancel ends
// the run cancelled, an expiry fails it with the same error. The pause
// goes and its checkpoints are closed with the original reason. It
// returns the ended run with cortex.ErrRunAlreadyDone for a cancel, or the
// expiry's error.
func (e *Engine) finishEnding(ctx context.Context, current *run.Run, saved *suspension.Progress, stale *suspension.Suspension) (*run.Run, error) {
	first := &suspension.Progress{RunID: current.ID, Cont: saved.Cont, Started: slices.Clone(saved.Started), Done: slices.Clone(saved.Done), Ending: saved.Ending}
	claimed, err := e.claimContinuableToEnd(ctx, current, first)
	if err != nil {
		return nil, fmt.Errorf("claim run %s to end it: %w", current.ID, err)
	}
	runCtx := cortex.WithScope(ctx, claimed.Scope)
	owesReply(claimed)
	switch saved.Ending.Kind {
	case suspension.EndingCancel:
		if cancelErr := e.cancelClaimed(runCtx, claimed); cancelErr != nil {
			return nil, cancelErr
		}
		return claimed, fmt.Errorf("cortex: continue run %s: the run was cancelled: %w: %w", claimed.ID, cortex.ErrRunNotContinuable, cortex.ErrRunAlreadyDone)
	default:
		expired := fmt.Errorf("%w: %s", cortex.ErrSuspensionExpired, saved.Ending.Reason)
		if failErr := e.persistFailure(runCtx, claimed, claimed.AgentID, expired); failErr != nil {
			return nil, errors.Join(expired, failErr)
		}
		if stale != nil {
			if delErr := e.store.DeleteSuspension(runCtx, claimed.ID); delErr != nil && !errors.Is(delErr, cortex.ErrNotSuspended) {
				e.logError("delete suspension of an expired run", delErr, log.String("run_id", claimed.ID.String()))
			}
		}
		e.closePendingCheckpoints(runCtx, claimed.ID, "nobody answered in time, so the run was failed and there is nothing left to decide")
		return claimed, expired
	}
}

// failReclaimed claims a reclaimable run and fails it with cause. The
// pause it was claimed from, if any, is dropped and its checkpoints closed,
// since nothing will ever decide them now. The claim takes this worker's
// lease when it can, so a run whose failure cannot be written lapses and
// is reclaimed again, counting towards the limit.
func (e *Engine) failReclaimed(ctx context.Context, current *run.Run, stale *suspension.Suspension, cause error) (*run.Run, error) {
	claimed, err := e.claimContinuableToEnd(ctx, current, nil)
	if err != nil {
		return nil, errors.Join(cause, fmt.Errorf("claim run %s to fail it: %w", current.ID, err))
	}
	runCtx := cortex.WithScope(ctx, claimed.Scope)
	owesReply(claimed)
	failErr := e.failResume(runCtx, claimed, claimed.AgentID, cause)
	if stale != nil {
		e.dropStalePause(runCtx, stale)
	}
	e.closePendingCheckpoints(runCtx, claimed.ID, "the run could not be continued after its worker stopped, so it was failed and there is nothing left to decide")
	return claimed, failErr
}

// dropStalePause deletes the pause a reclaimed run was claimed from and
// closes the checkpoints opened for it. The run has moved past that pause,
// and one suspension per run is what lets it pause again.
func (e *Engine) dropStalePause(ctx context.Context, stale *suspension.Suspension) {
	if err := e.store.DeleteSuspension(ctx, stale.RunID); err != nil && !errors.Is(err, cortex.ErrNotSuspended) {
		e.logError("delete the pause of a reclaimed run", err, log.String("run_id", stale.RunID.String()))
	}
	cps, err := e.store.ListPending(ctx, &checkpoint.ListFilter{RunID: stale.RunID.String()})
	if err != nil {
		e.logError("list the checkpoints of a reclaimed run", err, log.String("run_id", stale.RunID.String()))
		return
	}
	for _, cp := range cps {
		if opened, ok := cp.Metadata[checkpointSuspensionKey].(string); ok && opened == stale.ID.String() {
			e.closeCheckpoint(ctx, cp, supersededReason)
		}
	}
}

// rebuildReclaimed rebuilds the loop state from saved, settles the step
// the previous worker was in, and checks the run may re-enter the loop. It
// returns the context the loop runs on. A run that paused again on its
// replayed calls comes back as the resumption's paused run with no loop
// to run. Any failure has already ended the run, which comes back as
// ended when there is one to report.
func (e *Engine) rebuildReclaimed(ctx context.Context, r *run.Run, saved *suspension.Progress) (context.Context, *resumption, *run.Run, error) {
	ag, err := e.store.Get(ctx, r.AgentID)
	if err != nil {
		return nil, nil, nil, e.failResume(ctx, r, r.AgentID, fmt.Errorf("resolve agent: %w", err))
	}
	// A running run's row keeps the accounting it was created or last
	// paused with. What it used since lives in its progress, which only
	// the lease holder writes, so that is the run's accounting now.
	r.Usage, r.StepCount, r.TokensUsed = saved.Cont.Usage, saved.Cont.StepIndex, saved.Cont.TokensUsed
	ctx, ag, st, cfg, err := e.restoreLoopState(ctx, r, ag, saved.Cont)
	if err != nil {
		return nil, nil, nil, err
	}
	if checkErr := e.checkMessages(ctx, ag, r, cfg.Publication, st.messages); checkErr != nil {
		return nil, nil, nil, e.stopPublication(ctx, r, st, checkErr)
	}
	st.stepRows = e.savedStepRows(ctx, r.ID, st.stepIndex)

	pending, err := e.replayStep(ctx, ag, r, st, cfg, saved)
	if err != nil {
		return nil, nil, r, err
	}
	if pending.any() {
		st.provenance = r.Provenance
		reason, suspendErr := e.suspend(ctx, r, pending, st.continuation(cfg))
		if suspendErr != nil {
			e.failRun(ctx, r, ag.ID, suspendErr, time.Time{})
			return nil, nil, r, fmt.Errorf("suspend run: %w", suspendErr)
		}
		return ctx, &resumption{ag: ag, cfg: cfg, run: r, state: st, paused: &pausedAgain{reason: reason, calls: pending.calls}}, nil, nil
	}
	st.provenance = r.Provenance
	rz, err := e.loopEntry(ctx, r, ag, st, cfg)
	if err != nil {
		return nil, nil, r, err
	}
	return ctx, rz, nil, nil
}

// pausedAgain is a reclaimed run that paused before it reached the loop:
// a replayed call needs an approval or an answer again.
type pausedAgain struct {
	reason suspension.SuspendReason
	calls  []suspension.PendingCall
}

// savedStepRows maps each step index at or past from to the step row a
// previous worker already wrote for it. A worker that stalled after the
// model answered wrote the row before it lost the run; the loop reuses
// that row rather than writing a second one for the same index.
func (e *Engine) savedStepRows(ctx context.Context, runID id.AgentRunID, from int) map[int]id.StepID {
	steps, err := e.store.ListSteps(ctx, runID)
	if err != nil {
		e.logError("list the steps of a reclaimed run", err, log.String("run_id", runID.String()))
		return nil
	}
	var rows map[int]id.StepID
	for _, s := range steps {
		if s.Index < from {
			continue
		}
		if rows == nil {
			rows = make(map[int]id.StepID)
		}
		rows[s.Index] = s.ID
	}
	return rows
}

// outstandingCalls returns the calls of the history's last assistant
// message that have no tool result after it, in the order the model made
// them. A history that does not end in that assistant message and its
// results has none.
func outstandingCalls(msgs []llm.Message) []llm.ToolCall {
	last := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			last = i
			break
		}
		if msgs[i].Role != "tool" {
			return nil
		}
	}
	if last < 0 || len(msgs[last].ToolCalls) == 0 {
		return nil
	}
	answered := make(map[string]bool, len(msgs)-last-1)
	for _, m := range msgs[last+1:] {
		answered[m.ToolCallID] = true
	}
	var out []llm.ToolCall
	for _, call := range msgs[last].ToolCalls {
		if !answered[call.ID] {
			out = append(out, call)
		}
	}
	return out
}

// replayStep settles the calls of the step a reclaimed run was in when its
// worker stopped, walking them in the order the model made them:
//
//   - a call in Done is answered with the result the previous worker
//     recorded, and does not run again;
//   - a call in Started and not in Done, whose tool is not retry-safe, was
//     handed to its handler and nobody knows what it did. The run stops
//     with cortex.ErrEffectUncertain before any call of the step runs
//     again;
//   - any other call runs again through executePreparedTool: it is
//     authorized again, and it may be left pending, which pauses the run.
//
// It returns the calls left pending. Results are recorded in the run's
// progress as they come in, as the loop records them.
func (e *Engine) replayStep(ctx context.Context, ag *agent.Config, r *run.Run, st *reactState, cfg resolvedConfig, saved *suspension.Progress) (pendingCalls, error) {
	var pending pendingCalls
	calls := outstandingCalls(st.messages)
	if len(calls) == 0 {
		return pending, nil
	}
	done := make(map[string]suspension.ProgressResult, len(saved.Done))
	for _, d := range saved.Done {
		done[d.CallID] = d
	}
	started := make(map[string]bool, len(saved.Started))
	for _, s := range saved.Started {
		started[s.ID] = true
	}
	for _, call := range calls {
		if _, ok := done[call.ID]; ok || !started[call.ID] || e.retrySafe[call.Name] {
			continue
		}
		return pending, e.stopUncertainReplay(ctx, r, st, call, saved.Owner)
	}

	ctx, cancelTools := runDeadline(ctx, cfg)
	defer cancelTools()
	ctx = withOperationalRuntime(ctx, r, cfg, st)
	stepID := e.stepForPendingCalls(ctx, r.ID, st.stepIndex)
	subject := cortex.Subject{Scope: cortex.ScopeFromContext(ctx), Principal: cortex.PrincipalFromContext(ctx), AgentID: ag.ID, RunID: r.ID}
	stepCtx := pending.collecting(ctx, r.ID)
	for _, call := range calls {
		if d, ok := done[call.ID]; ok {
			// A result recorded with the claim has not been through the
			// publication checks a resume runs on it, so every saved result
			// is checked again before the model sees it.
			approved, resultErr := e.checkToolResult(ctx, ag, r, cfg.Publication, d.Content, "tool_result")
			if resultErr != nil {
				return pending, e.stopPublication(ctx, r, st, resultErr)
			}
			st.usage.ToolCalls++
			st.recordToolResult(call.ID, progressToolOutcome(d.Outcome), verification.ToolLocal)
			st.messages = append(st.messages, llm.Message{Role: "tool", Content: string(approved), ToolCallID: call.ID})
			continue
		}
		if checkErr := e.checkToolCalls(ctx, ag, r, cfg.Publication, []llm.ToolCall{call}); checkErr != nil {
			return pending, e.stopPublication(ctx, r, st, checkErr)
		}
		startedAt := time.Now().UTC()
		result, outcome, wait := e.executePreparedTool(e.announceCall(stepCtx, r.ID, call), subject, call, st)
		if interruption(ctx, r.ID) != nil {
			return pending, leftResume(r, e.leaveRun(ctx, r, nil))
		}
		if outcome == outcomeUncertain {
			return pending, errors.Join(cortex.ErrEffectUncertain, e.finalizeUncertainEffect(ctx, r, st))
		}
		if outcome == outcomePending {
			pending.add(call, wait.reason, wait.ttl, wait.note)
			continue
		}
		approved, resultErr := e.checkToolResult(ctx, ag, r, cfg.Publication, result, "tool_result")
		if resultErr != nil {
			return pending, e.stopPublication(ctx, r, st, resultErr)
		}
		result = string(approved)
		completedAt := time.Now().UTC()
		if !stepID.IsNil() {
			row := &run.ToolCall{
				Entity: cortex.NewEntity(), ID: id.NewToolCallID(), StepID: stepID, RunID: r.ID,
				ToolName:    publicToolName(ctx, call.Name),
				Arguments:   publicGenerationText(r, call.Arguments),
				Result:      publicGenerationText(r, result),
				Error:       publicToolFailure(ctx, outcome),
				StartedAt:   &startedAt,
				CompletedAt: &completedAt,
			}
			if err := e.store.CreateToolCall(ctx, row); err != nil {
				e.logError("create replayed tool call", err, log.String("run_id", r.ID.String()))
			}
		}
		if outcome == outcomeCompleted {
			e.emitToolCompleted(ctx, r.ID, call.Name, result, completedAt.Sub(startedAt))
		}
		st.recordToolResult(call.ID, outcome, verification.ToolLocal)
		st.messages = append(st.messages, llm.Message{Role: "tool", Content: result, ToolCallID: call.ID})
		e.noteDone(ctx, r.ID, call.ID, result, "", outcome)
	}
	return pending, nil
}

// stopUncertainReplay ends a reclaimed run whose call was handed to its
// handler by owner and has no recorded outcome. The call is reported
// failed, the run records which call it was, and it stops incomplete with
// cortex.ErrEffectUncertain, exactly as a handler that reports an
// uncertain effect stops it.
func (e *Engine) stopUncertainReplay(ctx context.Context, r *run.Run, st *reactState, call llm.ToolCall, owner string) error {
	e.emitToolFailed(ctx, r.ID, call.Name, cortex.ErrEffectUncertain)
	meta := maps.Clone(r.Metadata)
	if meta == nil {
		meta = make(map[string]any, 2)
	}
	meta["uncertain_tool"] = call.Name
	meta["uncertain_call_id"] = call.ID
	r.Metadata = meta
	if owner == "" {
		owner = "unknown"
	}
	uncertain := fmt.Errorf("%w: tool %q was handed to its handler by worker %s and its outcome is unknown", cortex.ErrEffectUncertain, call.Name, owner)
	e.logWarn("stop a reclaimed run on a call with no recorded outcome", uncertain, log.String("run_id", r.ID.String()))
	return errors.Join(uncertain, e.finalizeUncertainEffect(ctx, r, st))
}

// progressToolOutcome reverses progressOutcome.
func progressToolOutcome(outcome string) toolOutcome {
	switch outcome {
	case "completed":
		return outcomeCompleted
	case "denied":
		return outcomeDenied
	default:
		return outcomeFailed
	}
}
