package engine

import (
	"context"
	"errors"
	"slices"
	"sync"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

// progressState is what a leased run has saved since its last boundary:
// the state at the boundary, the calls that are not retry-safe and were
// handed to their handler, and the calls of the step that finished.
//
// A run saves it at four points, the same in both loops and in a resume:
//
//  1. once the run exists (step 0), or once a paused run is claimed (the
//     state it paused with);
//  2. after a model response passes its checks and its assistant message
//     with tool calls is appended, at the new step index;
//  3. immediately before a call that is not retry-safe is handed to its
//     handler (it joins Started; a retry-safe call never does);
//  4. after each call's checked result (it joins Done).
//
// A replica that reclaims the run continues from Cont, treats every call
// in Started that has no result in Done as uncertain, and reuses Done.
type progressState struct {
	cont    suspension.Continuation
	started []suspension.PendingCall
	done    []suspension.ProgressResult
}

// progressCont is the state a snapshot stores: the loop's continuation,
// with the run's current provenance, copied so later appends to the loop's
// slices cannot reach a saved snapshot.
func progressCont(r *run.Run, st *reactState, cfg resolvedConfig) suspension.Continuation {
	cont := st.continuation(cfg)
	cont.Provenance = r.Provenance
	cont.Messages = cloneExecutionMessages(cont.Messages)
	cont.ToolResults = slices.Clone(cont.ToolResults)
	return cont
}

// saveBoundary is progress points 1 and 2: a new durable boundary, with
// Started and Done empty.
func (e *Engine) saveBoundary(ctx context.Context, runID id.AgentRunID, cont suspension.Continuation) {
	if l := leaseFor(ctx, runID); l != nil {
		_ = l.saveBoundary(cont) //nolint:errcheck // a lost lease cancels the run; anything else is logged
	}
}

// saveProgress is progress point 1 for a run claimed from a pause the old
// way: the snapshot it continues from, with what the claim answered.
func (e *Engine) saveProgress(ctx context.Context, runID id.AgentRunID, p *suspension.Progress) {
	if l := leaseFor(ctx, runID); l != nil {
		l.mu.Lock()
		l.progress = progressState{cont: p.Cont, started: slices.Clone(p.Started), done: slices.Clone(p.Done)}
		l.mu.Unlock()
		_ = l.save() //nolint:errcheck // a lost lease cancels the run; anything else is logged
	}
}

// noteStarted is progress point 3, and it fails closed: a call that is not
// retry-safe is dispatched only once its Started record is durable. A save
// that fails is retried once. If the retry fails too, or the lease is gone,
// it returns the error and the call must not be dispatched; the run is
// interrupted (errProgressUnsaved, or cortex.ErrRunLeaseLost) and the pass
// ends at the loop's next check without writing anything else.
func (e *Engine) noteStarted(ctx context.Context, runID id.AgentRunID, tc llm.ToolCall) error {
	l := leaseFor(ctx, runID)
	if l == nil || e.retrySafe[tc.Name] {
		return nil
	}
	l.mu.Lock()
	l.progress.started = append(l.progress.started, pendingCall(tc))
	l.mu.Unlock()
	err := l.save()
	if err != nil && !errors.Is(err, cortex.ErrRunLeaseLost) {
		err = l.save()
	}
	if err == nil {
		return nil
	}
	// The call was never recorded, and it will not run.
	l.mu.Lock()
	l.progress.started = l.progress.started[:len(l.progress.started)-1]
	l.mu.Unlock()
	if !errors.Is(err, cortex.ErrRunLeaseLost) {
		l.interrupt(errProgressUnsaved)
	}
	return err
}

// recordsStart reports whether a call is recorded as started before its
// handler gets it (progress point 3): on a leased run, for a builtin or
// registered tool that is not retry-safe.
func (e *Engine) recordsStart(ctx context.Context, runID id.AgentRunID, name string) bool {
	return leaseFor(ctx, runID) != nil && !e.retrySafe[name] && (isBuiltinTool(name) || e.hasRegisteredTool(name))
}

// toolAnnouncement is a ToolCalled held back until the call's Started
// record is durable. A call that is never dispatched because that record
// could not be saved announces nothing, so no subscriber sees a call
// start that nothing ever ends.
type toolAnnouncement struct {
	once sync.Once
	fire func()
}

type toolAnnouncementKey struct{}

// announceCall fires ToolCalled for tc, or, for a call recorded as started
// before dispatch, arranges for it to fire once the call gets that far.
// The returned context is the one to execute the call on.
//
// also, when given, goes out with the ToolCalled: the stream loop's
// tool_call event, so a stream consumer never hears of a call that is not
// dispatched either.
func (e *Engine) announceCall(ctx context.Context, runID id.AgentRunID, tc llm.ToolCall, also ...func()) context.Context {
	fire := func() {
		e.emitToolCalled(ctx, runID, tc.Name, tc.Arguments)
		for _, f := range also {
			f()
		}
	}
	if !e.recordsStart(ctx, runID, tc.Name) {
		fire()
		return ctx
	}
	return context.WithValue(ctx, toolAnnouncementKey{}, &toolAnnouncement{fire: fire})
}

// announced fires the ToolCalled announceCall held back, once. A call
// reaches it when it is recorded as started, or when it ends without
// being dispatched for any other reason (a denial, a pause, a failure),
// each of which still owes the call its start before its end.
func announced(ctx context.Context) {
	if a, ok := ctx.Value(toolAnnouncementKey{}).(*toolAnnouncement); ok {
		a.once.Do(a.fire)
	}
}

// noteDone is progress point 4, for one call's checked result. A result
// that arrives after the run was interrupted is discarded by the caller
// before it gets here, so its call stays in Started.
func (e *Engine) noteDone(ctx context.Context, runID id.AgentRunID, callID, content, errText string, outcome toolOutcome) {
	l := leaseFor(ctx, runID)
	if l == nil {
		return
	}
	result := suspension.ProgressResult{CallID: callID, Content: content, Error: errText, Outcome: progressOutcome(outcome)}
	l.mu.Lock()
	// A call the claim recorded with its answer is replaced by the checked
	// result, never listed twice.
	if i := slices.IndexFunc(l.progress.done, func(d suspension.ProgressResult) bool { return d.CallID == callID }); i >= 0 {
		l.progress.done[i] = result
	} else {
		l.progress.done = append(l.progress.done, result)
	}
	l.mu.Unlock()
	_ = l.save() //nolint:errcheck // a lost lease cancels the run; anything else is logged
}

func progressOutcome(outcome toolOutcome) string {
	switch outcome {
	case outcomeCompleted:
		return "completed"
	case outcomeDenied:
		return "denied"
	default:
		return "failed"
	}
}

// saveBoundary starts a new boundary at cont and saves it.
func (l *runLease) saveBoundary(cont suspension.Continuation) error {
	l.mu.Lock()
	l.progress = progressState{cont: cont}
	l.mu.Unlock()
	return l.save()
}

// save writes the run's snapshot as it stands. ErrRunLeaseLost means the
// run is no longer this worker's: the run's context is cancelled with
// that cause, exactly as when the heartbeat finds it. Any other failure
// is logged, and the previous snapshot stands.
func (l *runLease) save() error {
	l.mu.Lock()
	p := &suspension.Progress{
		RunID:   l.runID,
		Owner:   l.owner,
		Cont:    l.progress.cont,
		Started: slices.Clone(l.progress.started),
		Done:    slices.Clone(l.progress.done),
	}
	l.mu.Unlock()
	ctx, cancel := context.WithTimeout(l.base, leaseWriteTimeout)
	defer cancel()
	err := l.e.progress.SaveProgress(ctx, p)
	if errors.Is(err, cortex.ErrRunLeaseLost) {
		l.interrupt(cortex.ErrRunLeaseLost)
		return err
	}
	if err != nil {
		l.e.logWarn("save run progress", err, log.String("run_id", l.runID.String()))
	}
	return err
}

// dropProgress deletes the snapshot of a run that has ended or paused.
// Postgres and SQLite cascade a deleted run, but nothing deletes a run
// that ends, and Mongo cascades nothing, so every such path calls this.
//
// With leases off no snapshot is ever written, so there is nothing to
// delete. Every engine sharing a store uses the same lease setting.
func (e *Engine) dropProgress(ctx context.Context, r *run.Run) {
	if e.progress == nil || e.leaseTTL == 0 || r.Provenance.VerificationRequired || r.Provenance.TaskClaimID != "" {
		return
	}
	base := context.WithoutCancel(ctx)
	if !r.Scope.IsZero() {
		base = cortex.WithScope(base, r.Scope)
	}
	writeCtx, cancel := context.WithTimeout(base, leaseWriteTimeout)
	defer cancel()
	if err := e.progress.DeleteProgress(writeCtx, r.ID); err != nil {
		e.logError("delete run progress", err, log.String("run_id", r.ID.String()))
	}
}
