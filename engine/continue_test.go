package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	sqlitestore "github.com/xraph/cortex/store/sqlite"
)

// continueLLM asks for its scripted tool calls on the first request and
// answers every later one with content. It counts requests and keeps a
// copy of each one's messages, so a test can say whether a decision
// called the model and what the model saw after it.
type continueLLM struct {
	mu       sync.Mutex
	first    []llm.ToolCall
	requests [][]llm.Message
}

func (c *continueLLM) next(req *llm.Request) *llm.Response {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, slices.Clone(req.Messages))
	if len(c.requests) == 1 && len(c.first) > 0 {
		return &llm.Response{ToolCalls: c.first, FinishReason: "tool_calls"}
	}
	return &llm.Response{Content: "done", FinishReason: "stop"}
}

func (c *continueLLM) Complete(_ context.Context, req *llm.Request) (*llm.Response, error) {
	return c.next(req), nil
}

func (c *continueLLM) CompleteStream(_ context.Context, req *llm.Request) (llm.Stream, error) {
	return &outcomeStream{response: c.next(req)}, nil
}

func (c *continueLLM) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

func (c *continueLLM) lastRequest() []llm.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.requests) == 0 {
		return nil
	}
	return c.requests[len(c.requests)-1]
}

// readyRecorder records every RunReady notice and the scope its ctx
// carried.
type readyRecorder struct {
	mu      sync.Mutex
	runs    []id.AgentRunID
	notices []run.ReadyNotice
	scopes  []cortex.Scope
}

func (r *readyRecorder) Name() string { return "ready-recorder" }

func (r *readyRecorder) OnRunReady(ctx context.Context, _ id.AgentID, runID id.AgentRunID, n run.ReadyNotice) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, runID)
	r.notices = append(r.notices, n)
	r.scopes = append(r.scopes, cortex.ScopeFromContext(ctx))
	return nil
}

func (r *readyRecorder) snapshot() ([]id.AgentRunID, []run.ReadyNotice, []cortex.Scope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.runs), slices.Clone(r.notices), slices.Clone(r.scopes)
}

// continueFixture is one engine over SQLite whose model asks for the gated
// tool once, an authorizer that escalates it, and a JSON principal codec.
type continueFixture struct {
	ctx   context.Context
	st    store.Store
	e     *Engine
	model *continueLLM
	ready *readyRecorder
	tools *toolEventRecorder

	mu         sync.Mutex
	ran        atomic.Int32
	principals []any
}

func newContinueFixture(t *testing.T, opts ...Option) *continueFixture {
	t.Helper()
	ctx := cortex.WithScope(context.Background(), approvalScope())
	return buildContinueFixture(ctx, t, newApprovalStore(ctx, t), []llm.ToolCall{{ID: "call-1", Name: gatedTool().Name, Arguments: `{}`}}, opts...)
}

func buildContinueFixture(ctx context.Context, t *testing.T, st store.Store, first []llm.ToolCall, opts ...Option) *continueFixture {
	t.Helper()
	f := &continueFixture{ctx: ctx, st: st, model: &continueLLM{first: first}, ready: &readyRecorder{}, tools: &toolEventRecorder{}}
	base := []Option{
		WithStore(st),
		WithLLM(f.model),
		WithTool(gatedTool(), func(_ context.Context, inv cortex.Invocation) (string, error) {
			f.ran.Add(1)
			f.mu.Lock()
			f.principals = append(f.principals, inv.Principal)
			f.mu.Unlock()
			return "deleted", nil
		}),
		WithExternalTool(externalTool()),
		WithToolAuthorizer(escalatingAuthorizer{tool: gatedTool().Name}),
		WithPrincipalCodec(cortex.JSONPrincipalCodec[testPrincipal]()),
		WithExtension(f.ready),
		WithExtension(f.tools),
	}
	e, err := New(append(base, opts...)...)
	require.NoError(t, err)
	f.e = e
	return f
}

// pause starts a run as alice and returns it paused, with the checkpoint
// it opened when it paused for approval.
func (f *continueFixture) pause(t *testing.T) (*run.Run, *checkpoint.Checkpoint) {
	t.Helper()
	paused, err := f.e.RunAgent(as(f.ctx, alice), "assistant", "clean up", nil)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, paused.State)
	cps, err := f.e.ListPendingCheckpoints(f.ctx, &checkpoint.ListFilter{RunID: paused.ID.String()})
	require.NoError(t, err)
	if len(cps) == 0 {
		return paused, nil
	}
	require.Len(t, cps, 1)
	return paused, cps[0]
}

func (f *continueFixture) runState(t *testing.T, runID id.AgentRunID) *run.Run {
	t.Helper()
	r, err := f.st.GetRun(f.ctx, runID)
	require.NoError(t, err)
	return r
}

func (f *continueFixture) checkpointState(t *testing.T, cpID id.CheckpointID) *checkpoint.Checkpoint {
	t.Helper()
	cp, err := f.st.GetCheckpoint(f.ctx, cpID)
	require.NoError(t, err)
	return cp
}

func (f *continueFixture) seenPrincipals() []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.principals)
}

// toolMessage returns the tool message for call-1, the fixture's only
// call, in messages.
func toolMessage(messages []llm.Message) (string, bool) {
	for _, m := range messages {
		if m.Role == "tool" && m.ToolCallID == "call-1" {
			return m.Content, true
		}
	}
	return "", false
}

func TestDecideCheckpointApproveDoesNotRunModel(t *testing.T) {
	f := newContinueFixture(t)
	paused, cp := f.pause(t)
	calls := f.model.calls()

	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))

	require.Equal(t, calls, f.model.calls(), "a decision must not call the model")
	require.Zero(t, f.ran.Load(), "a decision must not dispatch the tool")
	require.Equal(t, run.StateReady, f.runState(t, paused.ID).State)
	decided := f.checkpointState(t, cp.ID)
	require.Equal(t, "resolved", decided.State)
	require.NotNil(t, decided.Decision)
	require.True(t, decided.Decision.Approved)
	require.Equal(t, "bob", decided.Decision.DecidedBy)

	runs, notices, scopes := f.ready.snapshot()
	require.Len(t, notices, 1)
	require.Equal(t, paused.ID, runs[0])
	require.Equal(t, run.ReadyApproval, notices[0].Source)
	require.Equal(t, cp.ID, notices[0].CheckpointID)
	require.False(t, notices[0].At.IsZero())
	require.False(t, notices[0].Renotified)
	require.Equal(t, approvalScope().Canonical(), scopes[0].Canonical())
}

func TestContinueRunAfterApprovalCompletes(t *testing.T) {
	f := newContinueFixture(t)
	paused, cp := f.pause(t)
	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))

	// The job continuing the run carries nobody's principal. The tool
	// still runs as alice, the run's caller.
	done, err := f.e.ContinueRun(f.ctx, paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, done.State)
	require.EqualValues(t, 1, f.ran.Load())
	require.Equal(t, []any{alice}, f.seenPrincipals())

	steps, err := f.e.ListSteps(f.ctx, paused.ID)
	require.NoError(t, err)
	found := false
	for _, step := range steps {
		calls, listErr := f.e.ListToolCalls(f.ctx, step.ID)
		require.NoError(t, listErr)
		for _, call := range calls {
			if call.ToolName == gatedTool().Name && call.Result == "deleted" {
				found = true
			}
		}
	}
	require.True(t, found, "the approved call left no tool call row")
	require.Equal(t, run.StateCompleted, f.runState(t, paused.ID).State)
}

func TestDecideCheckpointRejectFailsRun(t *testing.T) {
	f := newContinueFixture(t)
	paused, cp := f.pause(t)

	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{DecidedBy: "bob", Reason: "too risky"}))

	failed := f.runState(t, paused.ID)
	require.Equal(t, run.StateFailed, failed.State)
	require.Contains(t, failed.Error, "checkpoint rejected: too risky")
	_, notices, _ := f.ready.snapshot()
	require.Empty(t, notices)
	decided := f.checkpointState(t, cp.ID)
	require.Equal(t, "resolved", decided.State)
	require.False(t, decided.Decision.Approved)
	require.Zero(t, f.ran.Load())
}

func TestRejectionReturnsToModel(t *testing.T) {
	f := newContinueFixture(t, WithRejectionsReturnToModel())
	paused, cp := f.pause(t)

	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{DecidedBy: "alice", Reason: "too risky"}))
	require.Equal(t, run.StateReady, f.runState(t, paused.ID).State)
	_, notices, _ := f.ready.snapshot()
	require.Len(t, notices, 1)
	require.Equal(t, run.ReadyRejection, notices[0].Source)
	require.Equal(t, cp.ID, notices[0].CheckpointID)

	done, err := f.e.ContinueRun(f.ctx, paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, done.State)
	content, ok := toolMessage(f.model.lastRequest())
	require.True(t, ok, "the model saw no tool message for the rejected call")
	require.Contains(t, content, "rejected by alice: too risky")
	require.Zero(t, f.ran.Load(), "a rejected call must never be dispatched")
	require.Equal(t, 1, countEvent(f.tools.snapshot(), "denied"))
	require.Zero(t, countEvent(f.tools.snapshot(), "failed"))
	require.Zero(t, countEvent(f.tools.snapshot(), "completed"))
}

func TestRejectionWithoutReasonOrDecider(t *testing.T) {
	f := newContinueFixture(t, WithRejectionsReturnToModel())
	paused, cp := f.pause(t)

	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{}))
	_, err := f.e.ContinueRun(f.ctx, paused.ID)
	require.NoError(t, err)
	content, ok := toolMessage(f.model.lastRequest())
	require.True(t, ok)
	require.Contains(t, content, "rejected by a reviewer: no reason given")
}

func TestResolveCheckpointRejectionContinuesWithOption(t *testing.T) {
	t.Run("option", func(t *testing.T) {
		f := newContinueFixture(t, WithRejectionsReturnToModel())
		paused, cp := f.pause(t)
		require.NoError(t, f.e.ResolveCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{DecidedBy: "bob", Reason: "no"}))
		require.Equal(t, run.StateCompleted, f.runState(t, paused.ID).State)
		require.Zero(t, f.ran.Load())
	})
	t.Run("option without the capability", func(t *testing.T) {
		ctx := cortex.WithScope(context.Background(), approvalScope())
		f := buildContinueFixture(ctx, t, hiddenContinuationStore{newApprovalStore(ctx, t)}, []llm.ToolCall{{ID: "call-1", Name: gatedTool().Name, Arguments: `{}`}}, WithRejectionsReturnToModel())
		paused, cp := f.pause(t)
		require.NoError(t, f.e.ResolveCheckpoint(as(ctx, bob), cp.ID, checkpoint.Decision{DecidedBy: "bob", Reason: "no"}))
		require.Equal(t, run.StateCompleted, f.runState(t, paused.ID).State)
		content, ok := toolMessage(f.model.lastRequest())
		require.True(t, ok)
		require.Contains(t, content, "rejected by bob: no")
		require.Equal(t, 1, countEvent(f.tools.snapshot(), "denied"))
		require.Zero(t, f.ran.Load())
	})
	t.Run("default", func(t *testing.T) {
		f := newContinueFixture(t)
		paused, cp := f.pause(t)
		require.NoError(t, f.e.ResolveCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{DecidedBy: "bob", Reason: "no"}))
		require.Equal(t, run.StateFailed, f.runState(t, paused.ID).State)
		require.Zero(t, f.ran.Load())
	})
}

// flakyResolveStore loses the next checkpoint Resolve when armed.
type flakyResolveStore struct {
	*sqlitestore.Store
	fail atomic.Bool
}

func (s *flakyResolveStore) Resolve(ctx context.Context, cpID id.CheckpointID, d checkpoint.Decision) error {
	if s.fail.CompareAndSwap(true, false) {
		return errors.New("resolve lost")
	}
	return s.Store.Resolve(ctx, cpID, d)
}

func TestDecideCheckpointIsIdempotentAfterLostResolve(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), approvalScope())
	st := &flakyResolveStore{Store: newApprovalStore(ctx, t)}
	f := buildContinueFixture(ctx, t, st, []llm.ToolCall{{ID: "call-1", Name: gatedTool().Name, Arguments: `{}`}})
	paused, cp := f.pause(t)
	decision := checkpoint.Decision{Approved: true, DecidedBy: "bob"}

	st.fail.Store(true)
	require.Error(t, f.e.DecideCheckpoint(as(ctx, bob), cp.ID, decision))
	require.Equal(t, run.StateReady, f.runState(t, paused.ID).State)
	require.Equal(t, checkpointStatePending, f.checkpointState(t, cp.ID).State)
	_, notices, _ := f.ready.snapshot()
	require.Empty(t, notices, "the host heard about a run whose decision was not recorded")

	require.NoError(t, f.e.DecideCheckpoint(as(ctx, bob), cp.ID, decision))
	decided := f.checkpointState(t, cp.ID)
	require.Equal(t, "resolved", decided.State)
	require.True(t, decided.Decision.Approved)
	_, notices, _ = f.ready.snapshot()
	require.Len(t, notices, 1)

	done, err := f.e.ContinueRun(ctx, paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, done.State)
	require.EqualValues(t, 1, f.ran.Load())
}

func TestDecideCheckpointConcurrentApprovalsContinueOnce(t *testing.T) {
	f := newContinueFixture(t)
	paused, cp := f.pause(t)

	var wg sync.WaitGroup
	decideErrs := make([]error, 2)
	for i := range decideErrs {
		wg.Go(func() {
			decideErrs[i] = f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: fmt.Sprintf("op-%d", i)})
		})
	}
	wg.Wait()
	for _, err := range decideErrs {
		if err != nil {
			require.ErrorIs(t, err, cortex.ErrInvalidState)
		}
	}
	require.Equal(t, run.StateReady, f.runState(t, paused.ID).State)

	continueErrs := make([]error, 2)
	for i := range continueErrs {
		wg.Go(func() {
			_, continueErrs[i] = f.e.ContinueRun(f.ctx, paused.ID)
		})
	}
	wg.Wait()
	completed, refused := 0, 0
	for _, err := range continueErrs {
		switch {
		case err == nil:
			completed++
		case errors.Is(err, cortex.ErrRunNotContinuable):
			refused++
		default:
			t.Fatalf("ContinueRun: %v", err)
		}
	}
	require.Equal(t, 1, completed)
	require.Equal(t, 1, refused)
	require.EqualValues(t, 1, f.ran.Load())
	require.Equal(t, run.StateCompleted, f.runState(t, paused.ID).State)
}

func TestDecideCheckpointAfterExpiryFails(t *testing.T) {
	f := newContinueFixture(t, WithSuspensionTTL(40*time.Millisecond))
	paused, cp := f.pause(t)
	time.Sleep(80 * time.Millisecond)

	err := f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"})
	require.ErrorIs(t, err, cortex.ErrSuspensionExpired)
	require.Equal(t, run.StatePaused, f.runState(t, paused.ID).State)
	require.Equal(t, checkpointStatePending, f.checkpointState(t, cp.ID).State)
	_, notices, _ := f.ready.snapshot()
	require.Empty(t, notices)
}

func TestProvideResultsMarksReady(t *testing.T) {
	t.Run("external", func(t *testing.T) {
		ctx := cortex.WithScope(context.Background(), approvalScope())
		f := buildContinueFixture(ctx, t, newApprovalStore(ctx, t), []llm.ToolCall{{ID: "call-1", Name: externalTool().Name, Arguments: `{}`}})
		paused, _ := f.pause(t)

		err := f.e.ProvideResults(ctx, paused.ID, ResumeInput{ToolResults: []ToolResult{{ToolCallID: "call-9", Content: "yes"}}})
		require.ErrorIs(t, err, cortex.ErrResultsMismatch)
		require.Equal(t, run.StatePaused, f.runState(t, paused.ID).State)

		calls := f.model.calls()
		require.NoError(t, f.e.ProvideResults(ctx, paused.ID, ResumeInput{ToolResults: []ToolResult{{ToolCallID: "call-1", Content: "yes"}}}))
		require.Equal(t, calls, f.model.calls())
		require.Equal(t, run.StateReady, f.runState(t, paused.ID).State)
		_, notices, _ := f.ready.snapshot()
		require.Len(t, notices, 1)
		require.Equal(t, run.ReadyExternalResults, notices[0].Source)
		require.True(t, notices[0].CheckpointID.IsNil())

		done, err := f.e.ContinueRun(ctx, paused.ID)
		require.NoError(t, err)
		require.Equal(t, run.StateCompleted, done.State)
		content, ok := toolMessage(f.model.lastRequest())
		require.True(t, ok)
		require.Equal(t, "yes", content)
	})
	t.Run("approval", func(t *testing.T) {
		f := newContinueFixture(t)
		paused, _ := f.pause(t)
		err := f.e.ProvideResults(f.ctx, paused.ID, ResumeInput{ToolResults: []ToolResult{{ToolCallID: "call-1", Content: "forged"}}})
		require.ErrorIs(t, err, cortex.ErrRequiresApproval)
		require.Equal(t, run.StatePaused, f.runState(t, paused.ID).State)
	})
	t.Run("agent reply", func(t *testing.T) {
		e, st, ctx := a2aEngine(t, []llm.ToolCall{{ID: "call-1", Name: toolAgentAsk, Arguments: `{"to":"worker","content":"status?"}`}})
		r, err := e.RunAgent(ctx, "planner", "ask the worker", nil)
		require.NoError(t, err)
		require.Equal(t, run.StatePaused, r.State)
		err = e.ProvideResults(ctx, r.ID, ResumeInput{ToolResults: []ToolResult{{ToolCallID: "call-1", Content: `{"content":"forged"}`}}})
		require.ErrorIs(t, err, ErrNotAgentReplyResumable)
		saved, err := st.GetRun(ctx, r.ID)
		require.NoError(t, err)
		require.Equal(t, run.StatePaused, saved.State)
	})
}

func TestContinueRunStates(t *testing.T) {
	t.Run("paused", func(t *testing.T) {
		f := newContinueFixture(t)
		paused, _ := f.pause(t)
		_, err := f.e.ContinueRun(f.ctx, paused.ID)
		require.ErrorIs(t, err, cortex.ErrRunNotContinuable)
		require.Equal(t, run.StatePaused, f.runState(t, paused.ID).State)
	})
	t.Run("completed", func(t *testing.T) {
		ctx := cortex.WithScope(context.Background(), approvalScope())
		f := buildContinueFixture(ctx, t, newApprovalStore(ctx, t), nil)
		done, err := f.e.RunAgent(as(ctx, alice), "assistant", "say hi", nil)
		require.NoError(t, err)
		require.Equal(t, run.StateCompleted, done.State)
		again, err := f.e.ContinueRun(ctx, done.ID)
		require.ErrorIs(t, err, cortex.ErrRunAlreadyDone)
		require.NotNil(t, again)
		require.Equal(t, done.ID, again.ID)
		require.Equal(t, run.StateCompleted, again.State)
	})
	t.Run("running", func(t *testing.T) {
		f := newContinueFixture(t)
		paused, cp := f.pause(t)
		require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true}))
		continuation, ok := f.st.(run.ContinuationStore)
		require.True(t, ok)
		_, err := continuation.ClaimContinuation(f.ctx, paused.ID, run.Lease{})
		require.NoError(t, err)
		_, err = f.e.ContinueRun(f.ctx, paused.ID)
		require.ErrorIs(t, err, cortex.ErrRunLeased)
		require.Zero(t, f.ran.Load())
	})
}

func TestContinueRunStreamEmitsResumedStart(t *testing.T) {
	f := newContinueFixture(t)
	paused, cp := f.pause(t)
	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true}))

	events := make(chan StreamEvent, 64)
	require.NoError(t, f.e.ContinueRunStream(f.ctx, paused.ID, events))
	var got []StreamEvent
	for ev := range events {
		got = append(got, ev)
	}
	require.NotEmpty(t, got)
	require.Equal(t, EventRunStarted, got[0].Type)
	require.Equal(t, true, got[0].Data["resumed"])
	require.Equal(t, paused.ID.String(), got[0].Data["run_id"])
	require.Equal(t, EventDone, got[len(got)-1].Type)
	require.EqualValues(t, 1, f.ran.Load())
}

func TestContinueRunRevokedPrincipalFailsRun(t *testing.T) {
	codec := newSwitchCodec()
	f := newContinueFixture(t, WithPrincipalCodec(codec))
	paused, cp := f.pause(t)
	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))
	calls := f.model.calls()

	codec.failDecode(fmt.Errorf("alice left the workspace: %w", cortex.ErrPrincipalRevoked))
	_, err := f.e.ContinueRun(f.ctx, paused.ID)
	require.ErrorIs(t, err, cortex.ErrPrincipalRevoked)
	failed := f.runState(t, paused.ID)
	require.Equal(t, run.StateFailed, failed.State)
	require.Contains(t, failed.Error, cortex.ErrPrincipalRevoked.Error())
	require.Zero(t, f.ran.Load())
	require.Equal(t, calls, f.model.calls())
}

func TestContinueRunTransientPrincipalErrorLeavesRunReady(t *testing.T) {
	codec := newSwitchCodec()
	f := newContinueFixture(t, WithPrincipalCodec(codec))
	paused, cp := f.pause(t)
	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))

	codec.failDecode(errors.New("identity service down"))
	_, err := f.e.ContinueRun(f.ctx, paused.ID)
	require.ErrorIs(t, err, cortex.ErrPrincipalUnavailable)
	require.NotErrorIs(t, err, cortex.ErrPrincipalRevoked)
	require.Equal(t, run.StateReady, f.runState(t, paused.ID).State)
	require.Zero(t, f.ran.Load())

	codec.failDecode(nil)
	done, err := f.e.ContinueRun(f.ctx, paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, done.State)
	require.Equal(t, []any{alice}, f.seenPrincipals())
}

// A run failed for a revoked principal leaves nothing pending in the
// decision queue, on either path an approval reaches it by.
func TestRevokedPrincipalClosesPendingCheckpoint(t *testing.T) {
	revoked := fmt.Errorf("alice left the workspace: %w", cortex.ErrPrincipalRevoked)
	t.Run("continue after a lost resolve", func(t *testing.T) {
		ctx := cortex.WithScope(context.Background(), approvalScope())
		st := &flakyResolveStore{Store: newApprovalStore(ctx, t)}
		codec := newSwitchCodec()
		f := buildContinueFixture(ctx, t, st, []llm.ToolCall{{ID: "call-1", Name: gatedTool().Name, Arguments: `{}`}}, WithPrincipalCodec(codec))
		paused, cp := f.pause(t)
		st.fail.Store(true)
		require.Error(t, f.e.DecideCheckpoint(as(ctx, bob), cp.ID, checkpoint.Decision{Approved: true}))
		require.Equal(t, run.StateReady, f.runState(t, paused.ID).State)

		codec.failDecode(revoked)
		_, err := f.e.ContinueRun(ctx, paused.ID)
		require.ErrorIs(t, err, cortex.ErrPrincipalRevoked)
		require.Equal(t, run.StateFailed, f.runState(t, paused.ID).State)
		require.NotEqual(t, checkpointStatePending, f.checkpointState(t, cp.ID).State)
	})
	t.Run("resolve without the capability", func(t *testing.T) {
		ctx := cortex.WithScope(context.Background(), approvalScope())
		codec := newSwitchCodec()
		f := buildContinueFixture(ctx, t, hiddenContinuationStore{newApprovalStore(ctx, t)}, []llm.ToolCall{{ID: "call-1", Name: gatedTool().Name, Arguments: `{}`}}, WithPrincipalCodec(codec))
		paused, cp := f.pause(t)
		codec.failDecode(revoked)
		err := f.e.ResolveCheckpoint(as(ctx, bob), cp.ID, checkpoint.Decision{Approved: true})
		require.ErrorIs(t, err, cortex.ErrPrincipalRevoked)
		require.Equal(t, run.StateFailed, f.runState(t, paused.ID).State)
		require.NotEqual(t, checkpointStatePending, f.checkpointState(t, cp.ID).State)
		require.Zero(t, f.ran.Load())
	})
}

func TestResolveCheckpointStillBlocksUntilRunEnds(t *testing.T) {
	f := newContinueFixture(t)
	paused, cp := f.pause(t)

	require.NoError(t, f.e.ResolveCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))
	require.Equal(t, run.StateCompleted, f.runState(t, paused.ID).State)
	require.EqualValues(t, 1, f.ran.Load())
	decided := f.checkpointState(t, cp.ID)
	require.Equal(t, "resolved", decided.State)
	require.True(t, decided.Decision.Approved)
	require.Equal(t, "bob", decided.Decision.DecidedBy)
	require.ErrorIs(t, f.e.ResolveCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true}), cortex.ErrInvalidState)

	// A decision recorded by DecideCheckpoint is not decided again by
	// ResolveCheckpoint: the run stays ready for its job.
	g := newContinueFixture(t)
	ready, readyCP := g.pause(t)
	require.NoError(t, g.e.DecideCheckpoint(as(g.ctx, bob), readyCP.ID, checkpoint.Decision{Approved: true}))
	require.ErrorIs(t, g.e.ResolveCheckpoint(as(g.ctx, bob), readyCP.ID, checkpoint.Decision{Approved: true}), cortex.ErrInvalidState)
	require.Equal(t, run.StateReady, g.runState(t, ready.ID).State)
	require.Zero(t, g.ran.Load())
}

func TestCancelRunCancelsReadyRun(t *testing.T) {
	f := newContinueFixture(t)
	paused, cp := f.pause(t)
	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true}))

	require.NoError(t, f.e.CancelRun(f.ctx, paused.ID))
	require.Equal(t, run.StateCancelled, f.runState(t, paused.ID).State)
	_, err := f.st.GetSuspension(f.ctx, paused.ID)
	require.Error(t, err, "a cancelled run kept its suspension")

	_, err = f.e.ContinueRun(f.ctx, paused.ID)
	require.ErrorIs(t, err, cortex.ErrRunAlreadyDone)
	require.Zero(t, f.ran.Load())
}

// hiddenContinuationStore is a store.Store whose run.ContinuationStore
// methods are out of reach, like a custom store written before them.
type hiddenContinuationStore struct{ store.Store }

func TestContinuationUnavailableWithoutCapability(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), approvalScope())
	f := buildContinueFixture(ctx, t, hiddenContinuationStore{newApprovalStore(ctx, t)}, []llm.ToolCall{{ID: "call-1", Name: gatedTool().Name, Arguments: `{}`}})
	paused, cp := f.pause(t)

	require.ErrorIs(t, f.e.DecideCheckpoint(as(ctx, bob), cp.ID, checkpoint.Decision{Approved: true}), cortex.ErrContinuationUnavailable)
	require.ErrorIs(t, f.e.ProvideResults(ctx, paused.ID, ResumeInput{ToolResults: []ToolResult{{ToolCallID: "call-1", Content: "x"}}}), cortex.ErrContinuationUnavailable)
	_, err := f.e.ContinueRun(ctx, paused.ID)
	require.ErrorIs(t, err, cortex.ErrContinuationUnavailable)
	require.Equal(t, run.StatePaused, f.runState(t, paused.ID).State)
	require.Equal(t, checkpointStatePending, f.checkpointState(t, cp.ID).State)

	require.NoError(t, f.e.ResolveCheckpoint(as(ctx, bob), cp.ID, checkpoint.Decision{Approved: true}))
	require.Equal(t, run.StateCompleted, f.runState(t, paused.ID).State)
	require.EqualValues(t, 1, f.ran.Load())
	require.Equal(t, []any{alice}, f.seenPrincipals())
}
