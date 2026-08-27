package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

// ErrNotAgentReplyResumable is returned when something other than the
// messaging bus tries to answer a run that is waiting on a peer agent.
var ErrNotAgentReplyResumable = errors.New("cortex: run is waiting on an agent reply")

// ToolResult is one pending call's outcome, reported by whoever executed
// it while the run was paused.
//
// Error exists so a caller whose own execution failed can say so instead
// of dressing the failure up as a result. That is what keeps the
// one-terminal-event-per-call rule true across a pause: a resumed call
// emits ToolCompleted or ToolFailed, never both and never neither.
type ToolResult struct {
	// ToolCallID is the pending call's id, verbatim.
	ToolCallID string `json:"tool_call_id"`
	// Content is what the tool returned. It goes back to the model as
	// the tool message for this call.
	Content string `json:"content,omitempty"`
	// Error, when non-empty, means the call did not produce a result.
	// The model is told so in the same error payload shape an
	// engine-side tool failure produces, and Content is ignored.
	Error string `json:"error,omitempty"`
	// Execute says the call was never run and the engine should run it
	// now. It is what an approval produces: a call the authorizer
	// escalated was stopped before dispatch, so a human granting it hands
	// back permission, not a result, and somebody still has to execute
	// the tool.
	//
	// It exists so approval keeps the one-result-per-pending-call rule
	// rather than bending it. Content and Error are ignored when it is
	// set, and the authorizer is not consulted a second time: it already
	// answered, and a human answered after it.
	//
	// Only a tool the engine can run can be executed this way. An
	// external tool is answered by supplying its result, since the engine
	// is not the thing that runs it.
	Execute bool `json:"execute,omitempty"`
}

// ResumeInput is everything a caller supplies to continue a suspended
// run. One result per pending call, no more and no fewer.
type ResumeInput struct {
	ToolResults []ToolResult `json:"tool_results"`
}

// resumption is the rehydrated run a claim produced, ready for whichever
// loop the caller asked for.
type resumption struct {
	ag        *agent.Config
	cfg       resolvedConfig
	run       *run.Run
	state     *reactState
	startedAt time.Time
}

// Resume continues a suspended run synchronously once every pending call
// has a result.
//
// It answers an external-tool pause. A run paused for approval is not
// resumable this way: it is waiting on a decision, and the decision is
// what ResolveCheckpoint carries. Resuming one here is
// cortex.ErrRequiresApproval and the run stays paused.
//
// It re-derives authority from the STORED scope rather than the caller's,
// re-enters the loop at the step the suspension recorded, and restores
// the history boundary and the session from the continuation instead of
// recomputing either. Resuming with a different scope, a fresh session,
// or a rebuilt message list are three different ways to corrupt a run
// that looked fine at the call site and went wrong several turns later.
func (e *Engine) Resume(ctx context.Context, runID id.AgentRunID, in ResumeInput) (*run.Run, error) {
	return e.resume(ctx, runID, in, resumeSourcePublic)
}

// resumeSource is who is answering a paused run, which is the thing a
// public caller must not be able to claim for itself.
//
// It replaced an `approved bool` when agent-reply pauses arrived. Two
// callers with two authorities was expressible as a boolean; three is
// where a boolean starts lying, because "not approved" would have to mean
// both "an ordinary caller" and "the message bus", which are the two
// things that most need telling apart.
type resumeSource int

const (
	// resumeSourcePublic is an ordinary caller through Resume.
	resumeSourcePublic resumeSource = iota
	// resumeSourceApproval is ResolveCheckpoint, reaching here having
	// read a pending checkpoint for the run.
	resumeSourceApproval
	// resumeSourceAgentReply is the messaging bus, reaching here having
	// claimed the pending-ask row that proves the reply is genuine.
	resumeSourceAgentReply
)

// resume is Resume with the one thing a public caller must not be able to
// say: which authority this is arriving under.
func (e *Engine) resume(ctx context.Context, runID id.AgentRunID, in ResumeInput, source resumeSource) (*run.Run, error) {
	ctx, rz, err := e.claimForResume(ctx, runID, in, source)
	if err != nil {
		return nil, err
	}
	return e.continueReAct(ctx, rz.ag, rz.cfg, rz.run, rz.state, rz.run.Input, rz.startedAt)
}

// ResumeStream is Resume over the streaming loop. The channel is closed
// when execution completes, same as StreamAgent's.
func (e *Engine) ResumeStream(ctx context.Context, runID id.AgentRunID, in ResumeInput, events chan<- StreamEvent) error {
	ctx, rz, err := e.claimForResume(ctx, runID, in, resumeSourcePublic)
	if err != nil {
		close(events)
		return err
	}

	go func() {
		defer close(events)

		// A resumed stream announces itself as started, because that is
		// the event every existing consumer already waits for before it
		// will render anything. A brand-new event type would be silently
		// dropped by all of them; the flag is there for the ones that
		// draw a timeline and care about the difference.
		events <- StreamEvent{Type: EventRunStarted, Data: map[string]any{
			"run_id":   rz.run.ID.String(),
			"agent_id": rz.ag.ID.String(),
			"resumed":  true,
		}}

		e.continueStreamReAct(ctx, rz.ag, rz.cfg, rz.run, rz.state, rz.run.Input, rz.startedAt, events)
	}()
	return nil
}

// claimForResume takes ownership of a paused run and rebuilds everything
// the loop needs to continue it. The returned context carries the run's
// own scope.
//
// Once ClaimSuspension succeeds the run is running and it belongs to this
// call: every exit below either hands the loop a runnable state or fails
// the run. Returning an error with the run left running would be the
// wedge the claim's expiry guard exists to prevent, reached from the
// other side.
func (e *Engine) claimForResume(ctx context.Context, runID id.AgentRunID, in ResumeInput, source resumeSource) (context.Context, *resumption, error) {
	if e.store == nil {
		return nil, nil, cortex.ErrNoStore
	}
	if cortex.ScopeFromContext(ctx).IsZero() {
		return nil, nil, cortex.ErrNoScope
	}

	// The results are checked twice, and the first check is the one that
	// is kind rather than correct. A caller that got the call ids wrong
	// has its run left paused and can retry, because nothing has moved
	// yet. Checking only after the claim would fail the run over a
	// malformed request, which is a cliff external-tool integrations
	// would fall off constantly. The authoritative check is the one
	// below, against the suspension the claim actually returned.
	if susp, err := e.store.GetSuspension(ctx, runID); err == nil {
		if vErr := validateResults(susp.Pending, in.ToolResults); vErr != nil {
			return nil, nil, vErr
		}
		if aErr := checkResumeAuthority(susp.Reason, source); aErr != nil {
			return nil, nil, aErr
		}
	}

	// The run is read BEFORE the claim, and a read that does not come
	// back ends the resume here. Nothing has moved at that point: the run
	// is still paused, its suspension is still claimable, and the caller
	// can try again. Reading only after the claim is what used to strand
	// a run on a store hiccup, since a claimed run nobody fails is
	// running forever.
	//
	// The caller's scope is enough for this read. A claim matches on a
	// scope prefix, so a caller who can claim this run can read it.
	snapshot, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return nil, nil, fmt.Errorf("load run to resume: %w", err)
	}

	susp, err := e.store.ClaimSuspension(ctx, runID)
	if err != nil {
		return nil, nil, fmt.Errorf("claim suspension: %w", err)
	}

	// Continue under the scope the run STARTED with, not the caller's. A
	// claim matches on a scope prefix, so a caller one level broader than
	// the run reaches this line legitimately, and everything the
	// continued run writes has to land where the run's own earlier writes
	// did rather than wherever the resumer happened to be standing.
	ctx = cortex.WithScope(ctx, susp.Scope)

	r := e.claimedRun(ctx, runID, snapshot)

	ag, err := e.store.Get(ctx, r.AgentID)
	if err != nil {
		return nil, nil, e.failResume(ctx, r, r.AgentID, fmt.Errorf("resolve agent: %w", err))
	}

	if err := validateResults(susp.Pending, in.ToolResults); err != nil {
		return nil, nil, e.failResume(ctx, r, ag.ID, err)
	}
	if err := checkResumeAuthority(susp.Reason, source); err != nil {
		return nil, nil, e.failResume(ctx, r, ag.ID, err)
	}

	st := stateFromContinuation(susp.Cont)
	e.recordResumedCalls(ctx, r, susp, in.ToolResults, st)

	// The suspension goes before the loop re-enters, not after the run
	// finishes. One suspension per run is what makes ClaimSuspension a
	// meaningful primitive, so a run that suspends again a step later
	// would collide with its own stale row and fail on a write that
	// should have succeeded. A row that outlives its claim is also a lie
	// to every reader: it says paused about a run that is running.
	if err := e.store.DeleteSuspension(ctx, runID); err != nil {
		return nil, nil, e.failResume(ctx, r, ag.ID, fmt.Errorf("delete claimed suspension: %w", err))
	}

	// The config comes from the continuation, never from a fresh merge of
	// the agent's. A rebuild would drop the run's own overrides: cfg.Tools
	// feeds resolveTools, so a run deliberately started with a narrowed
	// tool list would resume with the agent's full set, which is exactly
	// the authority widening this whole path exists to refuse. A rebuilt
	// MaxSteps misfires the guard just below it, too.
	cfg := configFromContinuation(susp.Cont.Config)
	// A continuation with no config at all is unusable, and it has to say
	// so in its own words. MaxSteps reads as 0, the guard just below
	// fires, and the run fails with ErrMaxStepsReached: an error that
	// means "this run used up its budget" and sends whoever reads it to
	// raise the budget, when nothing was ever going to run. Failing loud
	// is right; failing under somebody else's contract is not.
	if cfg.MaxSteps <= 0 {
		return nil, nil, e.failResume(ctx, r, ag.ID,
			fmt.Errorf("resume run %s: continuation carries no step budget: %w", runID, cortex.ErrInvalidContinuation))
	}
	if st.stepIndex >= cfg.MaxSteps {
		// Suspending on the last step stores a step index the budget has
		// no room for. Re-entering the loop here would fall straight
		// through it and complete the run with empty output, which reads
		// as a model that had nothing to say rather than a run that ran
		// out of steps. The results the caller worked to produce are
		// already recorded above and their terminal events have already
		// fired, so nothing is thrown away; what is refused is the
		// pretence that the run finished.
		return nil, nil, e.failResume(ctx, r, ag.ID,
			fmt.Errorf("resume run %s at step %d of %d: %w", runID, st.stepIndex, cfg.MaxSteps, cortex.ErrMaxStepsReached))
	}

	// The run's own start, not this call's, so a resumed run reports the
	// wall-clock time it actually took rather than only the part after
	// the pause.
	startedAt := time.Now().UTC()
	if r.StartedAt != nil {
		startedAt = *r.StartedAt
	}

	return ctx, &resumption{ag: ag, cfg: cfg, run: r, state: st, startedAt: startedAt}, nil
}

// claimedRun is the run a successful claim just took: read fresh, and
// falling back to the snapshot taken just before the claim when that
// read does not come back.
//
// The fallback is the difference between a store hiccup and a stranded
// run. Once a claim succeeds the run is running and only this call can
// move it, so returning without a terminal write leaves it running for
// good: every claim wants a paused run, so nothing takes it again, and
// the sweeper's orphan drop wants a terminal one, so nothing clears its
// suspension either.
//
// The snapshot is the same row read a moment earlier while the run was
// still paused, and nothing but a claim moves a paused run. Every caller
// here uses it for one thing, writing a terminal state onto a run that
// is ending, and the fields that decides are the ones this call sets
// itself.
func (e *Engine) claimedRun(ctx context.Context, runID id.AgentRunID, snapshot *run.Run) *run.Run {
	r, err := e.store.GetRun(ctx, runID)
	if err == nil {
		return r
	}
	e.logger.Error("read a claimed run, falling back to the pre-claim snapshot",
		log.String("run_id", runID.String()),
		log.String("error", err.Error()),
	)
	return snapshot
}

// failResume marks a claimed run failed and hands the reason back
// unwrapped, so callers can still match it with errors.Is.
func (e *Engine) failResume(ctx context.Context, r *run.Run, agentID id.AgentID, err error) error {
	e.failRun(ctx, r, agentID, err, time.Time{})
	return err
}

// recordResumedCalls closes the books on the calls the run paused for: a
// tool call row, exactly one terminal plugin event, and the tool message
// the model needs to see next to its own tool call.
//
// The terminal event is the part worth stating outright. v1.10.0
// established that every tool call fires exactly one of completed, failed
// or denied, and a pending call correctly fires none, because pending is
// not an ending. That only stays true if the resume supplies
// the missing one, so it does: a call the host executed emits
// ToolCompleted, a call the host could not execute emits ToolFailed. A
// tool call that suspends and then completes must not vanish from an
// audit trail, and one that suspends and then fails must not look like a
// success.
func (e *Engine) recordResumedCalls(ctx context.Context, r *run.Run, susp *suspension.Suspension, results []ToolResult, st *reactState) {
	byID := make(map[string]ToolResult, len(results))
	for _, res := range results {
		byID[res.ToolCallID] = res
	}

	stepID := e.stepForPendingCalls(ctx, r.ID, susp.Cont.StepIndex)
	startedAt := susp.CreatedAt

	// The subject an approved call is dispatched under. It is the run's
	// own, rebuilt from the run rather than the resumer, for the same
	// reason the whole resume path re-derives scope from the suspension.
	subject := cortex.Subject{
		Scope:     cortex.ScopeFromContext(ctx),
		Principal: cortex.PrincipalFromContext(ctx),
		AgentID:   r.AgentID,
		RunID:     r.ID,
	}

	// Pending order, not results order: the model sees its tool results
	// in the order it asked for them, and a caller is not obliged to
	// answer in that order.
	for _, p := range susp.Pending {
		// validateResults has already proven there is exactly one result
		// per pending id, so this lookup cannot miss.
		res := byID[p.ID]
		content, errText := e.resumedCallResult(ctx, subject, p, res)
		completedAt := time.Now().UTC()

		if !stepID.IsNil() {
			toolCall := &run.ToolCall{
				Entity:    cortex.NewEntity(),
				ID:        id.NewToolCallID(),
				StepID:    stepID,
				RunID:     r.ID,
				ToolName:  p.Name,
				Arguments: p.Arguments,
				Result:    content,
				Error:     errText,
				// The call really was outstanding from the moment the run
				// paused until now. Stamping it as instantaneous would
				// hide exactly the latency an external tool introduces.
				StartedAt:   &startedAt,
				CompletedAt: &completedAt,
			}
			if err := e.store.CreateToolCall(ctx, toolCall); err != nil {
				e.logger.Error("create resumed tool call", log.String("error", err.Error()))
			}
		}

		if errText != "" {
			e.extensions.EmitToolFailed(ctx, r.ID, p.Name, errors.New(errText))
		} else {
			e.extensions.EmitToolCompleted(ctx, r.ID, p.Name, content, completedAt.Sub(startedAt))
		}

		st.messages = append(st.messages, llm.Message{
			Role:       "tool",
			Content:    content,
			ToolCallID: p.ID,
		})
	}
}

// resumedCallResult turns one caller-supplied result into the pair the
// model and the audit trail need: what the tool said, and whether it
// failed. It is also where an approved call is finally executed.
//
// Approval does not produce a result. The call was stopped before
// dispatch, so there is nothing for whoever approved it to hand back:
// the decision produces permission, and the engine still has to run the
// tool. Hence Execute, and hence the bijection rule surviving intact,
// with one result per pending call whichever kind it is.
//
// The authorizer is deliberately not consulted again. It already
// answered, a human answered after it, and re-asking would escalate the
// same call forever.
func (e *Engine) resumedCallResult(ctx context.Context, s cortex.Subject, p suspension.PendingCall, res ToolResult) (content, errText string) {
	if !res.Execute {
		if res.Error != "" {
			// The same payload shape an engine-side failure feeds back,
			// so the model cannot tell where the tool ran from how its
			// failure is phrased.
			return jsonResult("error", res.Error), res.Error
		}
		return res.Content, ""
	}

	out, outcome, err := e.dispatchTool(ctx, s, llm.ToolCall{ID: p.ID, Name: p.Name, Arguments: p.Arguments})
	switch outcome {
	case outcomeCompleted:
		return out, ""
	case outcomeFailed:
		return out, err.Error()
	default:
		// An approved call that turns out to be external: the engine
		// cannot run it, and the only honest close is a failure that
		// says what to do instead. Suspending again is the alternative,
		// and it would mean a second place in the codebase where a run
		// pauses, which is the one thing this design refuses.
		msg := fmt.Sprintf("tool %q is external: an approved external call is answered by supplying its result, not by asking the engine to run it", p.Name)
		return jsonResult("error", msg), msg
	}
}

// stepForPendingCalls finds the step whose generation asked for the
// pending calls, so a resumed call's row lands on the same step as the
// sibling calls that ran before the run paused. The continuation's
// StepIndex is the NEXT step to run, so the step that made the calls is
// the one before it.
//
// A missing step costs the audit row and nothing else: step_id is a
// non-null foreign key, and the row is best-effort in the react loop
// already (a failed CreateStep is logged, not fatal). Losing the run over
// a bookkeeping row nobody can reconstruct would be the worse trade.
func (e *Engine) stepForPendingCalls(ctx context.Context, runID id.AgentRunID, nextStep int) id.StepID {
	if nextStep <= 0 {
		return id.StepID{}
	}
	steps, err := e.store.ListSteps(ctx, runID)
	if err != nil {
		e.logger.Error("list steps for resumed tool calls", log.String("error", err.Error()))
		return id.StepID{}
	}
	for _, s := range steps {
		if s.Index == nextStep-1 {
			return s.ID
		}
	}
	e.logger.Warn("no step found for resumed tool calls", log.String("run_id", runID.String()))
	return id.StepID{}
}

// checkResumeAuthority is the approval gate itself, and it covers the
// whole resume rather than the Execute flag on its results.
//
// A run suspended for approval is waiting on a decision, and Resume is
// in-process API with no decision behind it. The narrow version of this
// gate refused Execute and let everything else through, which left a
// caller free to answer an escalated call with invented Content: the
// model is fed output no tool produced, the run carries on, and the
// checkpoint stays pending forever with nobody the wiser. Nothing
// privileged is dispatched that way, so it is not the hole Execute was,
// but it is the same authority arriving by a different door.
//
// So an approval suspension is resumable only through ResolveCheckpoint,
// which reaches this having read a pending checkpoint for the run. Every
// other pause is unaffected: an external-tool suspension never went to a
// human in the first place, and Resume is exactly how it is meant to be
// answered.
func checkResumeAuthority(reason suspension.SuspendReason, source resumeSource) error {
	switch reason {
	case suspension.ReasonApproval:
		if source == resumeSourceApproval {
			return nil
		}
		return fmt.Errorf("%w: this run is waiting on a checkpoint decision, so resolve the checkpoint rather than resuming the run", cortex.ErrRequiresApproval)

	case suspension.ReasonAgentReply:
		// The same reasoning as an approval pause, with a different
		// authority behind it. A run waiting on a peer is answered by
		// whoever holds the ledger row proving the reply is genuine, and
		// that is the bus. A caller answering it here would feed the
		// model an answer no agent gave, and the run would carry on with
		// nobody the wiser.
		if source == resumeSourceAgentReply {
			return nil
		}
		return fmt.Errorf("%w: this run is waiting on another agent's reply, which only the messaging bus can deliver", ErrNotAgentReplyResumable)

	default:
		return nil
	}
}

// validateResults enforces the bijection between pending calls and the
// results a caller returns: one result per pending id, no extras, no
// duplicates.
//
// A reconstructed conversation carrying an assistant tool call with no
// matching result is rejected by the model as malformed, and that reaches
// a user as an unexplained failure several steps later, far from the
// cause. Catching it here costs one map.
func validateResults(pending []suspension.PendingCall, results []ToolResult) error {
	byID := make(map[string]int, len(pending))
	for _, p := range pending {
		byID[p.ID] = 0
	}
	for _, r := range results {
		n, ok := byID[r.ToolCallID]
		if !ok {
			return fmt.Errorf("%w: unexpected result for call id %q", cortex.ErrResultsMismatch, r.ToolCallID)
		}
		if n > 0 {
			return fmt.Errorf("%w: duplicate result for call id %q", cortex.ErrResultsMismatch, r.ToolCallID)
		}
		byID[r.ToolCallID] = 1
	}
	for callID, n := range byID {
		if n == 0 {
			return fmt.Errorf("%w: missing result for call id %q", cortex.ErrResultsMismatch, callID)
		}
	}
	return nil
}
