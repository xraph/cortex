package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/scopespy"
	"github.com/xraph/cortex/suspension"
)

// gatedTool is the registered tool every test here puts behind approval.
// It is registered, not external: the engine can run it, which is what
// makes "approved, now go run it" a meaningful outcome.
func gatedTool() llm.Tool {
	return llm.Tool{Name: "delete_everything", Description: "test-only tool that needs a human"}
}

// escalatingAuthorizer requires approval for one named tool and allows
// everything else, so a step can carry an escalated call and an ordinary
// one at the same time.
//
// The sentinel is wrapped rather than returned bare, because a host with
// something to say about why will wrap it, and the engine has to match
// with errors.Is rather than ==.
type escalatingAuthorizer struct {
	tool string
}

func (a escalatingAuthorizer) Visible(_ context.Context, _ cortex.Subject, tools []llm.Tool) []llm.Tool {
	return tools
}

func (a escalatingAuthorizer) Authorize(_ context.Context, _ cortex.Subject, call llm.ToolCall) error {
	if call.Name == a.tool {
		return fmt.Errorf("%s is destructive: %w", call.Name, cortex.ErrRequiresApproval)
	}
	return nil
}

// approvalScope is the scope every run in this file starts under.
func approvalScope() cortex.Scope {
	return cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}}
}

// approvalEngine wires an engine whose model calls the gated tool once
// and whose authorizer escalates exactly that tool.
func approvalEngine(t *testing.T, opts ...Option) *Engine {
	t.Helper()
	def := gatedTool()
	base := []Option{
		WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{
			{ID: "call-1", Name: def.Name, Arguments: `{"target":"everything"}`},
		}}),
		WithTool(def, func(_ context.Context, _ cortex.Invocation) (string, error) {
			return "deleted", nil
		}),
		WithToolAuthorizer(escalatingAuthorizer{tool: def.Name}),
	}
	e, err := New(append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// approvalRun drives one run whose single tool call is escalated for
// approval, and hands back the paused run with the store behind it.
func approvalRun(t *testing.T, opts ...Option) (*run.Run, *scopespy.Spy) {
	t.Helper()
	spy := scopespy.New()
	e := approvalEngine(t, append([]Option{WithStore(spy)}, opts...)...)

	r, err := e.RunAgent(cortex.WithScope(context.Background(), approvalScope()), "assistant", "clean up", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	return r, spy
}

// ──────────────────────────────────────────────────
// The third outcome
// ──────────────────────────────────────────────────

// TestExecuteTool_ApprovalIsPendingNotDenied is the whole difference
// between v1.10.0 and this release in one assertion. Before the loop
// could suspend, an authorizer asking for a human had nowhere to put the
// call, so returning it would have degraded to a denial: the model told
// the call was refused, and nobody ever asked.
func TestExecuteTool_ApprovalIsPendingNotDenied(t *testing.T) {
	def := gatedTool()
	rec := &toolEventRecorder{}
	e, err := New(
		WithStore(scopespy.New()),
		WithTool(def, func(_ context.Context, _ cortex.Invocation) (string, error) {
			return "deleted", nil
		}),
		WithToolAuthorizer(escalatingAuthorizer{tool: def.Name}),
		WithExtension(rec),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, outcome, reason := e.executeTool(context.Background(), cortex.Subject{}, llm.ToolCall{ID: "call-1", Name: def.Name})
	if outcome != outcomePending {
		t.Fatalf("outcome = %d, want outcomePending (%d); an escalation reported as anything else is a call nobody is asked about", outcome, outcomePending)
	}
	if reason != suspension.ReasonApproval {
		t.Errorf("pending reason = %q, want %q", reason, suspension.ReasonApproval)
	}
	if result != "" {
		t.Errorf("executeTool returned %q; nothing ran, so nothing may go back to the model as a result", result)
	}
	if got := countEvent(rec.snapshot(), "denied"); got != 0 {
		t.Errorf("ToolDenied fired %d times, want 0: the call was not refused, it is waiting", got)
	}
	// The handler must not have run. Approval that dispatches first and
	// asks afterwards is not approval.
	if got := countEvent(rec.snapshot(), "completed"); got != 0 {
		t.Errorf("ToolCompleted fired %d times, want 0: the call is still waiting on a human", got)
	}
}

// TestExecuteTool_AnOrdinaryDenialIsStillADenial keeps the two answers
// apart. Both arrive as an error from the same method, and treating every
// error as an escalation would turn every denial into a run that pauses
// forever waiting for someone to approve a call the authorizer already
// refused.
func TestExecuteTool_AnOrdinaryDenialIsStillADenial(t *testing.T) {
	def := gatedTool()
	rec := &toolEventRecorder{}
	e, err := New(
		WithStore(scopespy.New()),
		WithTool(def, func(_ context.Context, _ cortex.Invocation) (string, error) {
			return "deleted", nil
		}),
		WithToolAuthorizer(&recordingAuthorizer{authorizeErr: errors.New("not permitted")}),
		WithExtension(rec),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, outcome, reason := e.executeTool(context.Background(), cortex.Subject{}, llm.ToolCall{ID: "call-1", Name: def.Name})
	if outcome != outcomeDenied {
		t.Fatalf("outcome = %d, want outcomeDenied (%d)", outcome, outcomeDenied)
	}
	if reason != "" {
		t.Errorf("a denial carried the suspend reason %q; nothing is pending", reason)
	}
	if !strings.Contains(result, "not permitted") {
		t.Errorf("result = %q, want the denial reason fed back to the model", result)
	}
	if got := countEvent(rec.snapshot(), "denied"); got != 1 {
		t.Errorf("ToolDenied fired %d times, want 1", got)
	}
}

// ──────────────────────────────────────────────────
// The suspension and the checkpoint
// ──────────────────────────────────────────────────

// TestRunAgent_ApprovalSuspendsWithACheckpoint is the pause half of the
// end-to-end path: the run stops, and what stops it is visible to a
// human. The checkpoint package has had an entity, a store, REST
// endpoints and plugin hooks since v1.x, and until this release nothing
// in the loop ever created one.
func TestRunAgent_ApprovalSuspendsWithACheckpoint(t *testing.T) {
	r, spy := approvalRun(t)

	if r.State != run.StatePaused {
		t.Fatalf("run state = %q, want %q", r.State, run.StatePaused)
	}

	susps := spy.Suspensions()
	if len(susps) != 1 {
		t.Fatalf("the run wrote %d suspensions, want 1", len(susps))
	}
	if susps[0].Reason != suspension.ReasonApproval {
		t.Errorf("suspension reason = %q, want %q", susps[0].Reason, suspension.ReasonApproval)
	}
	if len(susps[0].Pending) != 1 || susps[0].Pending[0].ID != "call-1" {
		t.Fatalf("pending calls = %+v, want the escalated call", susps[0].Pending)
	}

	cps := spy.Checkpoints()
	if len(cps) != 1 {
		t.Fatalf("the run wrote %d checkpoints, want 1; a run paused for a decision nobody can see never continues", len(cps))
	}
	cp := cps[0]
	if cp.RunID != r.ID {
		t.Errorf("checkpoint RunID = %q, want the paused run %q", cp.RunID, r.ID)
	}
	if cp.AgentID != r.AgentID {
		t.Errorf("checkpoint AgentID = %q, want the run's agent %q", cp.AgentID, r.AgentID)
	}
	if cp.Scope.Canonical() != r.Scope.Canonical() {
		t.Errorf("checkpoint scope = %q, want the run's own %q", cp.Scope.Canonical(), r.Scope.Canonical())
	}
	if cp.State != "pending" {
		t.Errorf("checkpoint state = %q, want %q; ListPending selects on it, so anything else is invisible", cp.State, "pending")
	}
	if !strings.Contains(cp.Reason, gatedTool().Name) {
		t.Errorf("checkpoint reason = %q, want it to name the tool a human is being asked about", cp.Reason)
	}
	// The step that asked for the call, not the one the run resumes into.
	if cp.StepIndex != susps[0].Cont.StepIndex-1 {
		t.Errorf("checkpoint StepIndex = %d, want %d (the step whose generation made the call)", cp.StepIndex, susps[0].Cont.StepIndex-1)
	}
	if got, _ := cp.Metadata["suspension_id"].(string); got != susps[0].ID.String() {
		t.Errorf("checkpoint metadata suspension_id = %q, want the suspension it belongs to %q", got, susps[0].ID)
	}
}

// TestRunAgent_CheckpointOpensOnlyForApproval keeps the two pause reasons
// apart at the store. An external-tool pause is answered by the host that
// registered the tool, not by a person, and a queue full of rows nobody
// is meant to decide is a queue people stop reading.
//
// The two cases differ only in why the call is pending.
func TestRunAgent_CheckpointOpensOnlyForApproval(t *testing.T) {
	tests := []struct {
		name string
		want int
	}{
		{name: "approval", want: 1},
		{name: "external tool", want: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var spy *scopespy.Spy
			if tc.name == "approval" {
				_, spy = approvalRun(t)
			} else {
				_, spy = suspendingRun(t)
			}
			if got := len(spy.Checkpoints()); got != tc.want {
				t.Errorf("a run paused for %s wrote %d checkpoints, want %d", tc.name, got, tc.want)
			}
			if len(spy.Suspensions()) != 1 {
				t.Errorf("a run paused for %s wrote %d suspensions, want 1 either way", tc.name, len(spy.Suspensions()))
			}
		})
	}
}

// checkpointEventRecorder records the two checkpoint hooks. They have
// existed since v1.x and nothing has ever emitted them, so a wired-looking
// path is worth proving rather than assuming.
type checkpointEventRecorder struct {
	mu       sync.Mutex
	created  []string
	resolved []string
}

func (c *checkpointEventRecorder) Name() string { return "checkpoint-event-recorder" }

func (c *checkpointEventRecorder) OnCheckpointCreated(_ context.Context, cpID id.CheckpointID, _ id.AgentRunID, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created = append(c.created, cpID.String()+" "+reason)
	return nil
}

func (c *checkpointEventRecorder) OnCheckpointResolved(_ context.Context, cpID id.CheckpointID, decision string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resolved = append(c.resolved, cpID.String()+" "+decision)
	return nil
}

func (c *checkpointEventRecorder) createdEvents() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.created...)
}

func (c *checkpointEventRecorder) resolvedEvents() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.resolved...)
}

// TestRunAgent_ApprovalFiresTheCheckpointCreatedHook proves the hook is
// live rather than declared. plugin.Registry has carried
// EmitCheckpointCreated since v1.x with no caller anywhere in the repo.
func TestRunAgent_ApprovalFiresTheCheckpointCreatedHook(t *testing.T) {
	rec := &checkpointEventRecorder{}
	_, spy := approvalRun(t, WithExtension(rec))

	events := rec.createdEvents()
	if len(events) != 1 {
		t.Fatalf("OnCheckpointCreated fired %d times, want 1: %v", len(events), events)
	}
	cps := spy.Checkpoints()
	if len(cps) != 1 {
		t.Fatalf("the run wrote %d checkpoints, want 1", len(cps))
	}
	if !strings.HasPrefix(events[0], cps[0].ID.String()) {
		t.Errorf("the hook was given %q, want the id of the checkpoint that was written (%q)", events[0], cps[0].ID)
	}
	if !strings.Contains(events[0], gatedTool().Name) {
		t.Errorf("the hook was given %q, want a reason naming the tool", events[0])
	}
}

// TestSuspend_CheckpointWriteFailureDoesNotPauseTheRun is the write-order
// assertion for the row this task adds. A run paused for approval with no
// checkpoint behind it is waiting on a decision nobody can see: it never
// appears in ListPending, so nothing ever resolves it, and the only way
// out is a caller who knows to resume it by hand.
func TestSuspend_CheckpointWriteFailureDoesNotPauseTheRun(t *testing.T) {
	base := scopespy.New()
	base.FailCheckpointWrites(errors.New("store is down"))
	spy := &runStateSpy{Spy: base}
	e := approvalEngine(t, WithStore(spy))

	r, err := e.RunAgent(cortex.WithScope(context.Background(), approvalScope()), "assistant", "clean up", nil)
	if err == nil {
		t.Fatalf("RunAgent returned no error though the checkpoint could not be written; run = %+v", r)
	}
	if r != nil {
		t.Errorf("RunAgent returned a run alongside the error: %+v", r)
	}
	for _, st := range spy.wroteStates() {
		if st == run.StatePaused {
			t.Errorf("the run was persisted as %q with no checkpoint behind it; nobody can ever decide it. States written: %v", run.StatePaused, spy.wroteStates())
		}
	}
	// And the suspension goes with it: a row saying the run is paused
	// outlives a run that never paused otherwise.
	if got := len(base.Suspensions()); got != 0 {
		t.Errorf("%d suspensions survived a suspend that could not open its checkpoint", got)
	}
}

// checkpointOrphanSpy fails the run update that follows a successful
// suspension and checkpoint write, and records every Resolve the engine
// issues.
type checkpointOrphanSpy struct {
	*scopespy.Spy
	mu       sync.Mutex
	resolved []id.CheckpointID
}

func (c *checkpointOrphanSpy) UpdateRun(ctx context.Context, r *run.Run) error {
	if r.State == run.StatePaused {
		return errors.New("update rejected")
	}
	return c.Spy.UpdateRun(ctx, r)
}

func (c *checkpointOrphanSpy) Resolve(_ context.Context, cpID id.CheckpointID, _ checkpoint.Decision) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resolved = append(c.resolved, cpID)
	return nil
}

func (c *checkpointOrphanSpy) resolutions() []id.CheckpointID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]id.CheckpointID(nil), c.resolved...)
}

// TestSuspend_FailedStateFlipResolvesTheCheckpoint covers the far side of
// the same order. The checkpoint is written before the flip on purpose,
// so when the flip fails the row is attached to a run that is about to
// fail. Left pending it sits in an operator's queue forever asking about
// a run that never paused, and approving it would try to resume a failed
// run. There is no delete on the checkpoint store, so it is resolved
// against itself.
func TestSuspend_FailedStateFlipResolvesTheCheckpoint(t *testing.T) {
	base := scopespy.New()
	spy := &checkpointOrphanSpy{Spy: base}
	e := approvalEngine(t, WithStore(spy))

	r, err := e.RunAgent(cortex.WithScope(context.Background(), approvalScope()), "assistant", "clean up", nil)
	if err == nil {
		t.Fatalf("RunAgent returned no error though the run could not be paused; run = %+v", r)
	}

	cps := base.Checkpoints()
	if len(cps) != 1 {
		t.Fatalf("the run wrote %d checkpoints, want 1; there is nothing to clean up otherwise", len(cps))
	}
	got := spy.resolutions()
	if len(got) != 1 {
		t.Fatalf("Resolve called %d times, want 1: the checkpoint is stranded on a run that never paused", len(got))
	}
	if got[0] != cps[0].ID {
		t.Errorf("Resolve got checkpoint %q, want the stranded one %q", got[0], cps[0].ID)
	}
}

// TestRunAgent_AStepPendingForBothReasonsSuspendsForApproval settles what
// one suspension's reason says when a step pends for two of them. A
// suspension holds one reason, and that reason decides whether a
// checkpoint is opened at all, so external winning would mean a call
// somebody asked to have reviewed proceeds with nobody ever asked.
//
// The two cases differ only in which of the two calls the model asks for
// first. Order is the whole point: a rule that just keeps the last reason
// it saw gets one of them right by accident.
func TestRunAgent_AStepPendingForBothReasonsSuspendsForApproval(t *testing.T) {
	gated := gatedTool()
	ext := externalTool()
	gatedCall := llm.ToolCall{ID: "call-gated", Name: gated.Name, Arguments: "{}"}
	externalCall := llm.ToolCall{ID: "call-external", Name: ext.Name, Arguments: "{}"}

	tests := []struct {
		name  string
		calls []llm.ToolCall
	}{
		{name: "approval first", calls: []llm.ToolCall{gatedCall, externalCall}},
		{name: "external first", calls: []llm.ToolCall{externalCall, gatedCall}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spy := scopespy.New()
			e, err := New(
				WithStore(spy),
				WithLLM(&scriptedLLM{toolCalls: tc.calls}),
				WithExternalTool(ext),
				WithTool(gated, func(_ context.Context, _ cortex.Invocation) (string, error) { return "deleted", nil }),
				WithToolAuthorizer(escalatingAuthorizer{tool: gated.Name}),
			)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			r, err := e.RunAgent(cortex.WithScope(context.Background(), approvalScope()), "assistant", "clean up", nil)
			if err != nil {
				t.Fatalf("RunAgent: %v", err)
			}
			if r.State != run.StatePaused {
				t.Fatalf("run state = %q, want %q", r.State, run.StatePaused)
			}

			susps := spy.Suspensions()
			if len(susps) != 1 {
				t.Fatalf("the step wrote %d suspensions, want 1", len(susps))
			}
			if susps[0].Reason != suspension.ReasonApproval {
				t.Errorf("suspension reason = %q, want %q: the escalated call must not lose its checkpoint to a sibling", susps[0].Reason, suspension.ReasonApproval)
			}
			if len(susps[0].Pending) != 2 {
				t.Errorf("pending calls = %+v, want both", susps[0].Pending)
			}
			if got := len(spy.Checkpoints()); got != 1 {
				t.Errorf("the step wrote %d checkpoints, want 1", got)
			}
		})
	}
}

// ──────────────────────────────────────────────────
// Resolving the checkpoint
// ──────────────────────────────────────────────────

// TestResolveCheckpoint_ApprovalRunsTheCallAndFinishesTheRun is the
// second half of the end-to-end path, and the assertion that matters most
// is that the tool actually ran. An approved call was stopped before
// dispatch, so approval means "now go and run it", not "here is its
// result": a resume that only wrote a tool message would finish the run
// having told the model about work nobody did.
func TestResolveCheckpoint_ApprovalRunsTheCallAndFinishesTheRun(t *testing.T) {
	var ran int32
	rec := &checkpointEventRecorder{}
	tools := &toolEventRecorder{}
	spy := scopespy.New()
	def := gatedTool()

	e, err := New(
		WithStore(spy),
		WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{
			{ID: "call-1", Name: def.Name, Arguments: `{"target":"everything"}`},
		}}),
		WithTool(def, func(_ context.Context, _ cortex.Invocation) (string, error) {
			atomic.AddInt32(&ran, 1)
			return "deleted", nil
		}),
		WithToolAuthorizer(escalatingAuthorizer{tool: def.Name}),
		WithExtension(rec),
		WithExtension(tools),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := cortex.WithScope(context.Background(), approvalScope())
	paused, err := e.RunAgent(ctx, "assistant", "clean up", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if atomic.LoadInt32(&ran) != 0 {
		t.Fatal("the tool ran before anyone approved it")
	}

	cps := spy.Checkpoints()
	if len(cps) != 1 {
		t.Fatalf("the run wrote %d checkpoints, want 1", len(cps))
	}
	if resolveErr := e.ResolveCheckpoint(ctx, cps[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "operator"}); resolveErr != nil {
		t.Fatalf("ResolveCheckpoint: %v", resolveErr)
	}

	if got := atomic.LoadInt32(&ran); got != 1 {
		t.Errorf("the approved tool ran %d times, want 1; an approval that never dispatches tells the model about work nobody did", got)
	}
	resumed, err := spy.GetRun(ctx, paused.ID)
	if err != nil {
		t.Fatalf("reload the run: %v", err)
	}
	if resumed.State != run.StateCompleted {
		t.Errorf("run state = %q, want %q", resumed.State, run.StateCompleted)
	}

	// The row and the event the pause deliberately withheld.
	rows := spy.ToolCalls()
	if len(rows) != 1 {
		t.Fatalf("the approved call wrote %d tool call rows, want 1", len(rows))
	}
	if rows[0].Result != "deleted" {
		t.Errorf("tool call row result = %q, want the handler's own output", rows[0].Result)
	}
	if rows[0].Error != "" {
		t.Errorf("tool call row carries error %q, want none", rows[0].Error)
	}
	if got := countEvent(tools.snapshot(), "completed"); got != 1 {
		t.Errorf("ToolCompleted fired %d times, want 1: %v", got, tools.snapshot())
	}
	if got := countEvent(tools.snapshot(), "denied"); got != 0 {
		t.Errorf("ToolDenied fired %d times, want 0", got)
	}

	// The suspension is gone and the checkpoint is decided, so neither
	// says paused about a run that finished.
	if got := len(spy.Suspensions()); got != 0 {
		t.Errorf("%d suspensions survived a completed run", got)
	}
	stored, err := spy.GetCheckpoint(ctx, cps[0].ID)
	if err != nil {
		t.Fatalf("reload the checkpoint: %v", err)
	}
	if stored.State == "pending" {
		t.Error("the checkpoint is still pending after being decided; it stays in every operator's queue")
	}
	if len(rec.createdEvents()) != 1 {
		t.Errorf("OnCheckpointCreated fired %d times, want 1", len(rec.createdEvents()))
	}
	if got := rec.resolvedEvents(); len(got) != 1 || !strings.HasSuffix(got[0], "approved") {
		t.Errorf("OnCheckpointResolved got %v, want one event saying approved", got)
	}
}

// TestResolveCheckpoint_RejectionFailsTheRunWithTheDecisionsReason is the
// other answer. A rejected run must not sit paused forever, and the run
// has to carry the words the person who stopped it used: whoever finds
// the run later reads the reason, not a bare "rejected".
func TestResolveCheckpoint_RejectionFailsTheRunWithTheDecisionsReason(t *testing.T) {
	const reason = "we do not delete production data on a Friday"
	var ran int32
	rec := &checkpointEventRecorder{}
	spy := scopespy.New()
	def := gatedTool()

	e, err := New(
		WithStore(spy),
		WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{{ID: "call-1", Name: def.Name, Arguments: "{}"}}}),
		WithTool(def, func(_ context.Context, _ cortex.Invocation) (string, error) {
			atomic.AddInt32(&ran, 1)
			return "deleted", nil
		}),
		WithToolAuthorizer(escalatingAuthorizer{tool: def.Name}),
		WithExtension(rec),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := cortex.WithScope(context.Background(), approvalScope())
	paused, err := e.RunAgent(ctx, "assistant", "clean up", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	cps := spy.Checkpoints()
	if len(cps) != 1 {
		t.Fatalf("the run wrote %d checkpoints, want 1", len(cps))
	}

	if resolveErr := e.ResolveCheckpoint(ctx, cps[0].ID, checkpoint.Decision{DecidedBy: "operator", Reason: reason}); resolveErr != nil {
		t.Fatalf("ResolveCheckpoint: %v", resolveErr)
	}

	rejected, err := spy.GetRun(ctx, paused.ID)
	if err != nil {
		t.Fatalf("reload the run: %v", err)
	}
	if rejected.State != run.StateFailed {
		t.Fatalf("run state = %q, want %q; a rejected run left paused waits forever", rejected.State, run.StateFailed)
	}
	if !strings.Contains(rejected.Error, reason) {
		t.Errorf("run error = %q, want it to carry the decision's reason %q", rejected.Error, reason)
	}
	if got := atomic.LoadInt32(&ran); got != 0 {
		t.Errorf("the rejected tool ran %d times, want 0", got)
	}
	if got := len(spy.Suspensions()); got != 0 {
		t.Errorf("%d suspensions survived a rejected run", got)
	}
	if got := rec.resolvedEvents(); len(got) != 1 || !strings.HasSuffix(got[0], "rejected") {
		t.Errorf("OnCheckpointResolved got %v, want one event saying rejected", got)
	}
}

// TestResolveCheckpoint_ADecidedCheckpointCannotBeDecidedAgain stops the
// second decision. Approving a checkpoint somebody already rejected would
// try to resume a run the rejection failed, and the caller would read the
// claim's refusal rather than being told the decision was already made.
func TestResolveCheckpoint_ADecidedCheckpointCannotBeDecidedAgain(t *testing.T) {
	_, spy := approvalRun(t)
	e := approvalEngine(t, WithStore(spy))
	ctx := cortex.WithScope(context.Background(), approvalScope())

	cps := spy.Checkpoints()
	if len(cps) != 1 {
		t.Fatalf("the run wrote %d checkpoints, want 1", len(cps))
	}
	if resolveErr := e.ResolveCheckpoint(ctx, cps[0].ID, checkpoint.Decision{Reason: "no"}); resolveErr != nil {
		t.Fatalf("first ResolveCheckpoint: %v", resolveErr)
	}

	err := e.ResolveCheckpoint(ctx, cps[0].ID, checkpoint.Decision{Approved: true})
	if !errors.Is(err, cortex.ErrInvalidState) {
		t.Fatalf("second ResolveCheckpoint = %v, want ErrInvalidState", err)
	}
}

// countingEscalator is escalatingAuthorizer with a tally, for the one
// question that needs counting rather than an outcome.
type countingEscalator struct {
	tool  string
	calls int32
}

func (c *countingEscalator) Visible(_ context.Context, _ cortex.Subject, tools []llm.Tool) []llm.Tool {
	return tools
}

func (c *countingEscalator) Authorize(_ context.Context, _ cortex.Subject, call llm.ToolCall) error {
	atomic.AddInt32(&c.calls, 1)
	if call.Name == c.tool {
		return fmt.Errorf("%s is destructive: %w", call.Name, cortex.ErrRequiresApproval)
	}
	return nil
}

// TestResolveCheckpoint_AnApprovedCallIsNotSentBackToTheAuthorizer pins
// the one rule that makes an approval terminate. The authorizer already
// answered and a human answered after it; asking again would escalate the
// same call forever, and the run would pause on every resume.
func TestResolveCheckpoint_AnApprovedCallIsNotSentBackToTheAuthorizer(t *testing.T) {
	def := gatedTool()
	auth := &countingEscalator{tool: def.Name}
	spy := scopespy.New()

	e, err := New(
		WithStore(spy),
		WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{{ID: "call-1", Name: def.Name, Arguments: "{}"}}}),
		WithTool(def, func(_ context.Context, _ cortex.Invocation) (string, error) { return "deleted", nil }),
		WithToolAuthorizer(auth),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := cortex.WithScope(context.Background(), approvalScope())
	paused, err := e.RunAgent(ctx, "assistant", "clean up", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	cps := spy.Checkpoints()
	if len(cps) != 1 {
		t.Fatalf("the run wrote %d checkpoints, want 1", len(cps))
	}
	if resolveErr := e.ResolveCheckpoint(ctx, cps[0].ID, checkpoint.Decision{Approved: true}); resolveErr != nil {
		t.Fatalf("ResolveCheckpoint: %v", resolveErr)
	}

	if got := atomic.LoadInt32(&auth.calls); got != 1 {
		t.Errorf("Authorize ran %d times, want 1; an approved call re-asked would escalate forever", got)
	}
	resumed, err := spy.GetRun(ctx, paused.ID)
	if err != nil {
		t.Fatalf("reload the run: %v", err)
	}
	if resumed.State != run.StateCompleted {
		t.Errorf("run state = %q, want %q", resumed.State, run.StateCompleted)
	}
	if got := len(spy.Suspensions()); got != 0 {
		t.Errorf("%d suspensions survived; the approved run paused again", got)
	}
}

// TestResolveCheckpoint_AnApprovedExternalCallIsClosedAsAFailure covers
// the combination the engine cannot finish: the authorizer runs before
// the external check, so an external call can be escalated, and then
// approving it asks the engine to run a tool it does not own. Closing it
// as a failure that says what to do instead is the honest answer;
// suspending again would mean a second place in the codebase where a run
// pauses.
func TestResolveCheckpoint_AnApprovedExternalCallIsClosedAsAFailure(t *testing.T) {
	def := externalTool()
	tools := &toolEventRecorder{}
	spy := scopespy.New()

	e, err := New(
		WithStore(spy),
		WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{{ID: "call-1", Name: def.Name, Arguments: "{}"}}}),
		WithExternalTool(def),
		WithToolAuthorizer(escalatingAuthorizer{tool: def.Name}),
		WithExtension(tools),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := cortex.WithScope(context.Background(), approvalScope())
	paused, err := e.RunAgent(ctx, "assistant", "ask the human", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	cps := spy.Checkpoints()
	if len(cps) != 1 {
		t.Fatalf("the run wrote %d checkpoints, want 1", len(cps))
	}
	if resolveErr := e.ResolveCheckpoint(ctx, cps[0].ID, checkpoint.Decision{Approved: true}); resolveErr != nil {
		t.Fatalf("ResolveCheckpoint: %v", resolveErr)
	}

	rows := spy.ToolCalls()
	if len(rows) != 1 {
		t.Fatalf("the approved call wrote %d tool call rows, want 1", len(rows))
	}
	if !strings.Contains(rows[0].Error, "external") {
		t.Errorf("tool call row error = %q, want it to say the call is external and how to answer it", rows[0].Error)
	}
	if got := countEvent(tools.snapshot(), "failed"); got != 1 {
		t.Errorf("ToolFailed fired %d times, want 1: %v", got, tools.snapshot())
	}
	if got := countEvent(tools.snapshot(), "completed"); got != 0 {
		t.Errorf("ToolCompleted fired %d times, want 0; nothing ran", got)
	}
	// The run still ends rather than hanging on a call nobody can close.
	resumed, err := spy.GetRun(ctx, paused.ID)
	if err != nil {
		t.Fatalf("reload the run: %v", err)
	}
	if resumed.State != run.StateCompleted {
		t.Errorf("run state = %q, want %q", resumed.State, run.StateCompleted)
	}
}

// wedgeSpy refuses the write that marks a run failed, so a test can ask
// what a rejection does when the run will not move.
type wedgeSpy struct {
	*scopespy.Spy
}

func (w *wedgeSpy) UpdateRun(ctx context.Context, r *run.Run) error {
	if r.State == run.StateFailed {
		return errors.New("update rejected")
	}
	return w.Spy.UpdateRun(ctx, r)
}

// TestResolveCheckpoint_ARejectionThatCannotFailTheRunKeepsItResumable
// pins the order inside the rejection. The suspension is the only thing
// that can move a paused run, so it goes after the state write, never
// before: a delete that succeeded while the run stayed paused would leave
// a run nothing could ever pick up, which is the same wedge suspend's own
// write order exists to avoid, reached from the other side.
func TestResolveCheckpoint_ARejectionThatCannotFailTheRunKeepsItResumable(t *testing.T) {
	base := scopespy.New()
	spy := &wedgeSpy{Spy: base}
	e := approvalEngine(t, WithStore(spy))
	ctx := cortex.WithScope(context.Background(), approvalScope())

	if _, err := e.RunAgent(ctx, "assistant", "clean up", nil); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	cps := base.Checkpoints()
	if len(cps) != 1 {
		t.Fatalf("the run wrote %d checkpoints, want 1", len(cps))
	}

	err := e.ResolveCheckpoint(ctx, cps[0].ID, checkpoint.Decision{Reason: "no"})
	if got := len(base.Suspensions()); got != 1 {
		t.Errorf("%d suspensions left, want 1; the run is still paused and nothing else can move it", got)
	}
	if err == nil {
		t.Error("ResolveCheckpoint reported success though the run could not be failed; the caller has no idea it has to decide again")
	}
}
