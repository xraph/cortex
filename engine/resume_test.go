package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/scopespy"
	"github.com/xraph/cortex/suspension"
)

// scopeA is the scope every run in this file starts under, and scopeB is
// the one a resume arrives with when the test is about whose authority
// the continued run uses.
func scopeA() cortex.Scope {
	return cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_a"}}}
}

func scopeB() cortex.Scope {
	return cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_b"}}}
}

// answeringLLM asks for the external tool once and answers plainly after,
// which is the shape every resume test here needs: one suspension, then a
// run that can actually finish.
func answeringLLM(toolName string) *scriptedLLM {
	return &scriptedLLM{toolCalls: []llm.ToolCall{
		{ID: "call-1", Name: toolName, Arguments: `{"question":"proceed?"}`},
	}}
}

// suspendedFixture drives one run to a suspension and hands back
// everything a resume needs: the engine that owns it, the spy behind it,
// and the paused run.
func suspendedFixture(t *testing.T, spy interface {
	Suspensions() []*suspension.Suspension
}, e *Engine,
) *run.Run {
	t.Helper()
	ctx := cortex.WithScope(context.Background(), scopeA())
	r, err := e.RunAgent(ctx, "assistant", "ask the human", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if r.State != run.StatePaused {
		t.Fatalf("fixture run state = %q, want %q", r.State, run.StatePaused)
	}
	if len(spy.Suspensions()) != 1 {
		t.Fatalf("fixture wrote %d suspensions, want 1", len(spy.Suspensions()))
	}
	return r
}

// ──────────────────────────────────────────────────
// The bijection rule
// ──────────────────────────────────────────────────

// TestValidateResults covers the rule one case at a time. A conversation
// reconstructed with an assistant tool call that has no matching result
// is rejected by the model as malformed, and the user sees an unexplained
// failure several steps later, so each way of getting it wrong is worth
// naming on its own.
//
// Every case runs against the same two pending calls; only the results
// differ.
func TestValidateResults(t *testing.T) {
	pending := []suspension.PendingCall{
		{ID: "call-1", Name: "ask_human"},
		{ID: "call-2", Name: "ask_human"},
	}

	tests := []struct {
		name    string
		results []ToolResult
		wantErr bool
		wantMsg string
	}{
		{
			name:    "exact match passes",
			results: []ToolResult{{ToolCallID: "call-1"}, {ToolCallID: "call-2"}},
		},
		{
			name:    "extra result fails",
			results: []ToolResult{{ToolCallID: "call-1"}, {ToolCallID: "call-2"}, {ToolCallID: "call-3"}},
			wantErr: true,
			wantMsg: "unexpected",
		},
		{
			name:    "duplicate result fails",
			results: []ToolResult{{ToolCallID: "call-1"}, {ToolCallID: "call-1"}, {ToolCallID: "call-2"}},
			wantErr: true,
			wantMsg: "duplicate",
		},
		{
			name:    "missing result fails",
			results: []ToolResult{{ToolCallID: "call-1"}},
			wantErr: true,
			wantMsg: "missing",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateResults(pending, tc.results)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("validateResults = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, cortex.ErrResultsMismatch) {
				t.Fatalf("validateResults = %v, want it to wrap ErrResultsMismatch", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("validateResults = %q, want it to say which way the results are wrong (%q)", err, tc.wantMsg)
			}
		})
	}
}

// ──────────────────────────────────────────────────
// Continuing the run
// ──────────────────────────────────────────────────

// TestResume_ContinuesTheRunToCompletion is the base case: the caller
// answers the pending call, the loop picks up where it stopped, and the
// run finishes.
func TestResume_ContinuesTheRunToCompletion(t *testing.T) {
	spy := scopespy.New()
	e := mustResumeEngine(t, spy)
	r := suspendedFixture(t, spy, e)

	resumed, err := e.Resume(resumeCtx(scopeA()), r.ID, oneResult(t, spy, "the human said yes"))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.State != run.StateCompleted {
		t.Fatalf("resumed run state = %q, want %q", resumed.State, run.StateCompleted)
	}
	if resumed.Output != "done" {
		t.Errorf("resumed run output = %q, want the model's answer after the tool result", resumed.Output)
	}
	// The step budget carries across the pause: step 0 ran before the
	// suspension, step 1 after it.
	if resumed.StepCount != 2 {
		t.Errorf("resumed run StepCount = %d, want 2 (one step either side of the pause)", resumed.StepCount)
	}
}

// TestResume_FeedsTheToolResultBackToTheModel proves the result actually
// reaches the conversation the next request is built from. Without the
// tool message, the model sees its own tool call unanswered.
func TestResume_FeedsTheToolResultBackToTheModel(t *testing.T) {
	spy := scopespy.New()
	llmSpy := &requestRecordingLLM{scriptedLLM: answeringLLM(externalTool().Name)}
	e := mustResumeEngine(t, spy, WithLLM(llmSpy))
	r := suspendedFixture(t, spy, e)

	if _, err := e.Resume(resumeCtx(scopeA()), r.ID, oneResult(t, spy, "the human said yes")); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	last := llmSpy.lastRequest()
	if last == nil {
		t.Fatal("the resumed loop never called the model")
	}
	var found bool
	for _, m := range last.Messages {
		if m.Role == "tool" && m.ToolCallID == "call-1" && m.Content == "the human said yes" {
			found = true
		}
	}
	if !found {
		t.Errorf("the resumed request carried no tool result for call-1: %+v", last.Messages)
	}
}

// TestResume_DoesNotDuplicateHistory is the end-to-end proof the whole
// NewMessagesFrom fix chain exists for. v1.8.0 shipped a version that
// re-saved the loaded history every turn; agents went deaf after about
// six runs because the duplicated rows filled the fixed read window until
// it held nothing recent. A resume rebuilds its message list from the
// continuation, history included, so slicing from 0 brings the defect
// straight back.
func TestResume_DoesNotDuplicateHistory(t *testing.T) {
	const historyLen = 3
	history := make([]memory.Message, historyLen)
	for i := range history {
		history[i] = memory.Message{Role: "user", Content: "earlier turn"}
	}
	spy := &conversationSpy{Spy: scopespy.New(), history: history}
	e := mustResumeEngine(t, spy)
	r := suspendedFixture(t, spy, e)

	if _, err := e.Resume(resumeCtx(scopeA()), r.ID, oneResult(t, spy, "the human said yes")); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	saves := spy.saved()
	if len(saves) != 1 {
		t.Fatalf("SaveConversation ran %d times, want 1 (the suspended pass saves nothing)", len(saves))
	}
	for _, m := range saves[0] {
		if m.Content == "earlier turn" {
			t.Fatalf("the resume saved a message from the loaded history back as a new row: %+v", saves[0])
		}
	}

	// The other half of the same bug: a resume that reloaded the history
	// would also answer with a message list the run never built.
	if got := spy.loadCount(); got != 1 {
		t.Errorf("LoadConversation ran %d times, want 1; a resume takes its messages from the continuation, not the store", got)
	}
}

// TestResume_SavesIntoTheSessionTheRunStartedIn covers the second field
// the continuation carries for this. resolveSession lazily creates a
// default session, and the spy's ListSessions never finds one, so a
// resume that re-resolved would mint a second session and save this run's
// tail into it.
func TestResume_SavesIntoTheSessionTheRunStartedIn(t *testing.T) {
	spy := scopespy.New()
	e := mustResumeEngine(t, spy)
	r := suspendedFixture(t, spy, e)

	if _, err := e.Resume(resumeCtx(scopeA()), r.ID, oneResult(t, spy, "the human said yes")); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	var sessions int
	for _, c := range spy.Calls() {
		if c.Method == "CreateSession" {
			sessions++
		}
		if c.Method == "SaveConversation" && c.SessionID != r.SessionID {
			t.Errorf("SaveConversation carried session %q, want the run's own %q", c.SessionID, r.SessionID)
		}
	}
	if sessions != 1 {
		t.Errorf("CreateSession ran %d times, want 1; the resume re-resolved the session instead of restoring it", sessions)
	}
}

// ──────────────────────────────────────────────────
// Authority
// ──────────────────────────────────────────────────

// TestResume_ContinuesUnderTheStoredScopeNotTheResumers is the
// security-relevant one. The claim itself matches on a scope prefix, so a
// resume can legitimately arrive with authority that is not the run's;
// everything the continued run writes still has to land where the run's
// own earlier writes did.
func TestResume_ContinuesUnderTheStoredScopeNotTheResumers(t *testing.T) {
	spy := scopespy.New()
	e := mustResumeEngine(t, spy)
	r := suspendedFixture(t, spy, e)

	before := len(spy.Calls())
	if _, err := e.Resume(resumeCtx(scopeB()), r.ID, oneResult(t, spy, "the human said yes")); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	var seenClaim bool
	var checked int
	for _, c := range spy.Calls()[before:] {
		if c.Method == "ClaimSuspension" {
			seenClaim = true
			continue
		}
		if !seenClaim {
			// Reads before the claim are made with the caller's own
			// authority on purpose: that is what stops a resume reaching
			// a run it has no business touching at all.
			continue
		}
		checked++
		if c.Scope.Canonical() != scopeA().Canonical() {
			t.Errorf("%s after the claim carried scope %q, want the run's own %q", c.Method, c.Scope.Canonical(), scopeA().Canonical())
		}
	}
	if !seenClaim {
		t.Fatal("the resume never claimed the suspension")
	}
	if checked == 0 {
		t.Fatal("no store calls after the claim; this test proves nothing")
	}
}

// ──────────────────────────────────────────────────
// Refusals
// ──────────────────────────────────────────────────

// TestResume_MismatchedResultsLeaveTheRunPaused pins the order of the two
// validations. Rejecting a malformed request only after the claim would
// have flipped the run to running and then failed it, so a caller that
// got the ids wrong could never retry.
func TestResume_MismatchedResultsLeaveTheRunPaused(t *testing.T) {
	spy := scopespy.New()
	e := mustResumeEngine(t, spy)
	r := suspendedFixture(t, spy, e)

	before := len(spy.Calls())
	_, err := e.Resume(resumeCtx(scopeA()), r.ID, ResumeInput{
		ToolResults: []ToolResult{{ToolCallID: "not-the-pending-call"}},
	})
	if !errors.Is(err, cortex.ErrResultsMismatch) {
		t.Fatalf("Resume with the wrong call id = %v, want ErrResultsMismatch", err)
	}

	for _, c := range spy.Calls()[before:] {
		if c.Method == "ClaimSuspension" {
			t.Error("the run was claimed despite results that could never be applied; it can no longer be resumed")
		}
	}
	if len(spy.Suspensions()) != 1 {
		t.Errorf("the suspension is gone after a rejected resume; the run can never be resumed now")
	}
}

// TestResume_SecondResumeLosesTheRace covers the store contract from the
// engine side: whoever claims first continues the run, and the second
// caller is told the run is no longer suspended rather than handed a
// second copy of it.
func TestResume_SecondResumeLosesTheRace(t *testing.T) {
	spy := scopespy.New()
	e := mustResumeEngine(t, spy)
	r := suspendedFixture(t, spy, e)
	in := oneResult(t, spy, "the human said yes")

	if _, err := e.Resume(resumeCtx(scopeA()), r.ID, in); err != nil {
		t.Fatalf("first Resume: %v", err)
	}
	if _, err := e.Resume(resumeCtx(scopeA()), r.ID, in); !errors.Is(err, cortex.ErrNotSuspended) {
		t.Fatalf("second Resume = %v, want ErrNotSuspended", err)
	}
}

// expiredClaimSpy fails every claim the way a store does once the
// deadline has passed. The deadline itself is enforced in the store (its
// conformance suite proves that against all three backends); what belongs
// here is that Resume reports it and moves nothing.
type expiredClaimSpy struct {
	*scopespy.Spy
}

func (e *expiredClaimSpy) ClaimSuspension(_ context.Context, _ id.AgentRunID) (*suspension.Suspension, error) {
	return nil, cortex.ErrSuspensionExpired
}

func TestResume_ExpiredSuspensionIsReportedAndNothingIsWritten(t *testing.T) {
	base := scopespy.New()
	spy := &expiredClaimSpy{Spy: base}
	e := mustResumeEngine(t, spy)
	r := suspendedFixture(t, spy, e)

	before := len(base.Calls())
	_, err := e.Resume(resumeCtx(scopeA()), r.ID, oneResult(t, base, "too late"))
	if !errors.Is(err, cortex.ErrSuspensionExpired) {
		t.Fatalf("Resume of an expired suspension = %v, want ErrSuspensionExpired", err)
	}
	for _, c := range base.Calls()[before:] {
		if c.Method == "UpdateRun" || c.Method == "DeleteSuspension" || c.Method == "CreateToolCall" {
			t.Errorf("an expired resume still called %s; nothing about the run should have moved", c.Method)
		}
	}
}

// ──────────────────────────────────────────────────
// The pending calls themselves
// ──────────────────────────────────────────────────

// TestResume_FiresTheTerminalEventTheSuspendedCallNeverGot holds the
// v1.10.0 invariant across a pause. Task 3 fires nothing for a pending
// call, correctly, because pending is not an ending. That is only correct
// if the resume supplies the missing terminal event.
func TestResume_FiresTheTerminalEventTheSuspendedCallNeverGot(t *testing.T) {
	tests := []struct {
		name    string
		result  ToolResult
		want    string
		notWant string
	}{
		{
			name:    "a call the caller executed completes",
			result:  ToolResult{ToolCallID: "call-1", Content: "the human said yes"},
			want:    "completed",
			notWant: "failed",
		},
		{
			name:    "a call the caller could not execute fails",
			result:  ToolResult{ToolCallID: "call-1", Error: "the human never answered"},
			want:    "failed",
			notWant: "completed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spy := scopespy.New()
			rec := &toolEventRecorder{}
			e := mustResumeEngine(t, spy, WithExtension(rec))
			r := suspendedFixture(t, spy, e)

			if got := countEvent(rec.snapshot(), tc.want); got != 0 {
				t.Fatalf("Tool%s fired %d times before the resume, want 0", tc.want, got)
			}
			if _, err := e.Resume(resumeCtx(scopeA()), r.ID, ResumeInput{ToolResults: []ToolResult{tc.result}}); err != nil {
				t.Fatalf("Resume: %v", err)
			}

			events := rec.snapshot()
			if got := countEvent(events, tc.want); got != 1 {
				t.Errorf("Tool%s fired %d times, want exactly 1: %v", tc.want, got, events)
			}
			if got := countEvent(events, tc.notWant); got != 0 {
				t.Errorf("Tool%s fired %d times, want 0: %v", tc.notWant, got, events)
			}
			if got := countEvent(events, "called"); got != 1 {
				t.Errorf("ToolCalled fired %d times, want 1; the resume must not re-announce a call the model made before the pause: %v", got, events)
			}
		})
	}
}

// TestResume_WritesTheToolCallRowOnTheStepThatMadeTheCall covers the
// audit trail. Task 3 deliberately wrote no row for a pending call, since
// a row stamped complete for a call that never ran is a lie no resume can
// correct. The row is owed here instead, and it belongs to the step whose
// generation asked for the call, alongside any sibling that ran inline.
func TestResume_WritesTheToolCallRowOnTheStepThatMadeTheCall(t *testing.T) {
	spy := scopespy.New()
	e := mustResumeEngine(t, spy)
	r := suspendedFixture(t, spy, e)

	if _, err := e.Resume(resumeCtx(scopeA()), r.ID, oneResult(t, spy, "the human said yes")); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	rows := spy.ToolCalls()
	if len(rows) != 1 {
		t.Fatalf("the resume wrote %d tool call rows, want 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.ToolName != externalTool().Name || row.Result != "the human said yes" {
		t.Errorf("tool call row = %+v, want the external tool and the caller's result", row)
	}
	if row.Arguments != `{"question":"proceed?"}` {
		t.Errorf("tool call row arguments = %q, want the model's own arguments", row.Arguments)
	}

	var step0 id.StepID
	for _, s := range spy.Steps() {
		if s.Index == 0 {
			step0 = s.ID
		}
	}
	if step0.IsNil() {
		t.Fatal("no step 0 was recorded; the fixture never ran a generation")
	}
	if row.StepID != step0 {
		t.Errorf("tool call row StepID = %q, want step 0 (%q), the step whose generation asked for the call", row.StepID, step0)
	}
}

// TestResume_DeletesTheSuspensionBeforeTheLoopRuns is why the delete is
// not left until the run finishes. One suspension per run is what makes
// ClaimSuspension meaningful, so a run that suspends again a step later
// would collide with its own claimed row.
func TestResume_DeletesTheSuspensionBeforeTheLoopRuns(t *testing.T) {
	spy := scopespy.New()
	// Every step calls the external tool, so the resumed run suspends
	// again immediately. That second suspension is only writable if the
	// first row is already gone.
	e := mustResumeEngine(t, spy, WithLLM(&alwaysCallingLLM{tool: externalTool().Name}))
	r := suspendedFixture(t, spy, e)

	resumed, err := e.Resume(resumeCtx(scopeA()), r.ID, oneResult(t, spy, "the human said yes"))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.State != run.StatePaused {
		t.Fatalf("resumed run state = %q, want %q; the second external call should suspend it again", resumed.State, run.StatePaused)
	}

	susps := spy.Suspensions()
	if len(susps) != 1 {
		t.Fatalf("the store holds %d suspensions, want 1 (the new one, the claimed one deleted)", len(susps))
	}
	if len(susps[0].Pending) != 1 || susps[0].Pending[0].ID != "call-2" {
		t.Errorf("the surviving suspension is pending on %+v, want the second call", susps[0].Pending)
	}
}

// ──────────────────────────────────────────────────
// The final step
// ──────────────────────────────────────────────────

// lastStepAgentSpy hands back an agent with a one-step budget, so the run
// suspends on the last step it was ever allowed to take.
type lastStepAgentSpy struct {
	*scopespy.Spy
}

func (l *lastStepAgentSpy) GetByName(ctx context.Context, name string) (*agent.Config, error) {
	ag, err := l.Spy.GetByName(ctx, name)
	if err != nil {
		return nil, err
	}
	ag.MaxSteps = 1
	return ag, nil
}

func (l *lastStepAgentSpy) Get(ctx context.Context, agentID id.AgentID) (*agent.Config, error) {
	ag, err := l.Spy.Get(ctx, agentID)
	if err != nil {
		return nil, err
	}
	ag.MaxSteps = 1
	return ag, nil
}

// TestResume_FinalStepFailsRatherThanCompletingEmpty covers the edge the
// step budget creates. Suspending on the last step stores a step index
// the budget has no room for, so re-entering the loop falls straight
// through it. Completing the run with empty output would read as a model
// that had nothing to say rather than a run that ran out of steps, so the
// run fails instead, explicitly.
func TestResume_FinalStepFailsRatherThanCompletingEmpty(t *testing.T) {
	base := scopespy.New()
	spy := &lastStepAgentSpy{Spy: base}
	rec := &toolEventRecorder{}
	e := mustResumeEngine(t, spy, WithExtension(rec))
	r := suspendedFixture(t, spy, e)

	resumed, err := e.Resume(resumeCtx(scopeA()), r.ID, oneResult(t, base, "the human said yes"))
	if !errors.Is(err, cortex.ErrMaxStepsReached) {
		t.Fatalf("Resume at the step budget = (%v, %v), want ErrMaxStepsReached", resumed, err)
	}
	if resumed != nil {
		t.Errorf("Resume returned a run alongside the error: %+v", resumed)
	}

	// Failed, not completed, and above all not left running: the claim
	// already moved it out of paused, so a run nobody fails is a run
	// nothing can ever pick up again.
	persisted, err := base.GetRun(resumeCtx(scopeA()), r.ID)
	if err != nil {
		t.Fatalf("reload the resumed run: %v", err)
	}
	if persisted.State != run.StateFailed {
		t.Errorf("run ended as %q, want %q; a run left running after a claim can never be picked up again", persisted.State, run.StateFailed)
	}

	// The caller's work is not thrown away just because the run cannot
	// continue: the result is on the audit trail and its terminal event
	// fired.
	if got := countEvent(rec.snapshot(), "completed"); got != 1 {
		t.Errorf("ToolCompleted fired %d times, want 1; the executed call still has to be reported", got)
	}
	if len(base.ToolCalls()) != 1 {
		t.Errorf("the resume wrote %d tool call rows, want 1", len(base.ToolCalls()))
	}
	if len(base.Suspensions()) != 0 {
		t.Errorf("the suspension survived a resume that failed the run; it says paused about a failed run")
	}
}

// ──────────────────────────────────────────────────
// Streaming
// ──────────────────────────────────────────────────

// TestResumeStream_ContinuesTheRunToCompletion covers the second loop. A
// resume implemented only for the synchronous path would leave streaming
// hosts unable to answer the calls their own runs suspended for.
func TestResumeStream_ContinuesTheRunToCompletion(t *testing.T) {
	def := externalTool()
	spy := scopespy.New()
	e, err := New(
		WithStore(spy),
		WithLLM(scopespy.ToolCallingStreamLLM(def.Name)),
		WithExternalTool(def),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := cortex.WithScope(context.Background(), scopeA())
	events := make(chan StreamEvent, 64)
	if streamErr := e.StreamAgent(ctx, "assistant", "ask the human", nil, events); streamErr != nil {
		t.Fatalf("StreamAgent: %v", streamErr)
	}
	for range events {
		// Drain: the channel closes once the suspend has been persisted.
		_ = 0
	}

	susps := spy.Suspensions()
	if len(susps) != 1 {
		t.Fatalf("the streaming run wrote %d suspensions, want 1", len(susps))
	}

	resumeEvents := make(chan StreamEvent, 64)
	err = e.ResumeStream(resumeCtx(scopeA()), susps[0].RunID, ResumeInput{
		ToolResults: []ToolResult{{ToolCallID: susps[0].Pending[0].ID, Content: "the human said yes"}},
	}, resumeEvents)
	if err != nil {
		t.Fatalf("ResumeStream: %v", err)
	}

	var sawStarted, sawResumedFlag, sawDone bool
	for ev := range resumeEvents {
		switch ev.Type {
		case EventRunStarted:
			sawStarted = true
			if resumedFlag, ok := ev.Data["resumed"].(bool); ok && resumedFlag {
				sawResumedFlag = true
			}
		case EventDone:
			sawDone = true
		}
	}
	if !sawStarted {
		t.Error("the resumed stream emitted no run_started; consumers wait for it before rendering anything")
	}
	if !sawResumedFlag {
		t.Error("the resumed stream's run_started did not say it was a resume")
	}
	if !sawDone {
		t.Fatal("the resumed stream never emitted done; the run did not finish")
	}
}

// ──────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────

// mustResumeEngine builds an engine whose model calls the external tool
// once and then answers, over whichever store double the test brought.
func mustResumeEngine(t *testing.T, st store.Store, opts ...Option) *Engine {
	t.Helper()
	def := externalTool()
	base := []Option{
		WithStore(st),
		WithLLM(answeringLLM(def.Name)),
		WithExternalTool(def),
	}
	e, err := New(append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// resumeCtx builds the context a resume arrives with.
func resumeCtx(s cortex.Scope) context.Context {
	return cortex.WithScope(context.Background(), s)
}

// oneResult answers the single pending call the fixture suspended on,
// reading its id from the stored suspension rather than restating a
// literal the LLM double happens to use.
func oneResult(t *testing.T, spy interface {
	Suspensions() []*suspension.Suspension
}, content string,
) ResumeInput {
	t.Helper()
	susps := spy.Suspensions()
	if len(susps) != 1 || len(susps[0].Pending) != 1 {
		t.Fatalf("fixture has %d suspensions; oneResult needs exactly 1 with 1 pending call", len(susps))
	}
	return ResumeInput{ToolResults: []ToolResult{{ToolCallID: susps[0].Pending[0].ID, Content: content}}}
}

// requestRecordingLLM keeps the last request the loop built, which is the
// only place a tool result is observable before it reaches a provider.
type requestRecordingLLM struct {
	*scriptedLLM
	mu   sync.Mutex
	last *llm.Request
}

func (r *requestRecordingLLM) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	r.mu.Lock()
	cp := *req
	r.last = &cp
	r.mu.Unlock()
	return r.scriptedLLM.Complete(ctx, req)
}

func (r *requestRecordingLLM) lastRequest() *llm.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// alwaysCallingLLM never stops asking for the tool, so a resumed run
// suspends again on its next step.
type alwaysCallingLLM struct {
	mu   sync.Mutex
	tool string
	n    int
}

func (a *alwaysCallingLLM) Complete(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n++
	return &llm.Response{ToolCalls: []llm.ToolCall{
		{ID: fmt.Sprintf("call-%d", a.n), Name: a.tool, Arguments: "{}"},
	}}, nil
}

func (a *alwaysCallingLLM) CompleteStream(_ context.Context, _ *llm.Request) (llm.Stream, error) {
	return nil, errors.New("alwaysCallingLLM: CompleteStream not supported")
}

// conversationSpy records what SaveConversation was given and serves a
// fixed history, which is what a history-duplication assertion needs.
type conversationSpy struct {
	*scopespy.Spy
	history []memory.Message

	mu     sync.Mutex
	loads  int
	writes [][]memory.Message
}

func (c *conversationSpy) LoadConversation(ctx context.Context, agentID id.AgentID, sessionID id.SessionID, limit int) ([]memory.Message, error) {
	if _, err := c.Spy.LoadConversation(ctx, agentID, sessionID, limit); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.loads++
	c.mu.Unlock()
	return c.history, nil
}

func (c *conversationSpy) SaveConversation(ctx context.Context, agentID id.AgentID, sessionID id.SessionID, msgs []memory.Message) error {
	c.mu.Lock()
	c.writes = append(c.writes, append([]memory.Message(nil), msgs...))
	c.mu.Unlock()
	return c.Spy.SaveConversation(ctx, agentID, sessionID, msgs)
}

func (c *conversationSpy) saved() [][]memory.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]memory.Message(nil), c.writes...)
}

func (c *conversationSpy) loadCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loads
}
