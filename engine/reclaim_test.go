package engine_test

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
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/telemetry/telemetrytest"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

// reclaimLLM answers each Complete with the next scripted response and
// keeps every request, so a test can see the messages a reclaimed run sent.
type reclaimLLM struct {
	mu        sync.Mutex
	responses []*llm.Response
	requests  []*llm.Request
}

func (m *reclaimLLM) Complete(_ context.Context, req *llm.Request) (*llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, req)
	n := len(m.requests) - 1
	if n < len(m.responses) {
		return m.responses[n], nil
	}
	return &llm.Response{Content: "done", FinishReason: "stop"}, nil
}

func (m *reclaimLLM) CompleteStream(ctx context.Context, req *llm.Request) (llm.Stream, error) {
	resp, err := m.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	return &admissionStream{chunk: &llm.Chunk{Content: resp.Content, FinishReason: resp.FinishReason, ToolCalls: resp.ToolCalls}}, nil
}

func (m *reclaimLLM) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

// firstRequest is the first request the model got.
func (m *reclaimLLM) firstRequest(t *testing.T) *llm.Request {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	require.NotEmpty(t, m.requests)
	return m.requests[0]
}

// reclaimHooks records what a reclaiming engine announces.
type reclaimHooks struct {
	mu          sync.Mutex
	completed   int
	failed      int
	toolFailed  []string
	suspended   []suspension.Notice
	ready       []run.ReadyNotice
	readyAt     []time.Time
	reclaimable []run.ReclaimNotice
	reclaimAt   []time.Time
	scopes      []cortex.Scope
	reclaimedCh chan run.ReclaimNotice
}

func newReclaimHooks() *reclaimHooks {
	return &reclaimHooks{reclaimedCh: make(chan run.ReclaimNotice, 64)}
}

func (*reclaimHooks) Name() string { return "reclaim-hooks" }

func (h *reclaimHooks) OnRunCompleted(context.Context, id.AgentID, id.AgentRunID, string, time.Duration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.completed++
	return nil
}

func (h *reclaimHooks) OnRunFailed(context.Context, id.AgentID, id.AgentRunID, error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failed++
	return nil
}

func (h *reclaimHooks) OnToolFailed(_ context.Context, _ id.AgentRunID, name string, _ error) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.toolFailed = append(h.toolFailed, name)
	return nil
}

func (h *reclaimHooks) OnRunSuspended(_ context.Context, _ id.AgentID, _ id.AgentRunID, n suspension.Notice) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.suspended = append(h.suspended, n)
	return nil
}

func (h *reclaimHooks) OnRunReady(_ context.Context, _ id.AgentID, _ id.AgentRunID, n run.ReadyNotice) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ready = append(h.ready, n)
	h.readyAt = append(h.readyAt, time.Now())
	return nil
}

func (h *reclaimHooks) OnRunReclaimable(ctx context.Context, _ id.AgentID, _ id.AgentRunID, n run.ReclaimNotice) error {
	h.mu.Lock()
	h.reclaimable = append(h.reclaimable, n)
	h.reclaimAt = append(h.reclaimAt, time.Now())
	h.scopes = append(h.scopes, cortex.ScopeFromContext(ctx))
	h.mu.Unlock()
	select {
	case h.reclaimedCh <- n:
	default:
	}
	return nil
}

func (h *reclaimHooks) snapshot() reclaimHooks {
	h.mu.Lock()
	defer h.mu.Unlock()
	return reclaimHooks{
		completed:   h.completed,
		failed:      h.failed,
		toolFailed:  slices.Clone(h.toolFailed),
		suspended:   slices.Clone(h.suspended),
		ready:       slices.Clone(h.ready),
		readyAt:     slices.Clone(h.readyAt),
		reclaimable: slices.Clone(h.reclaimable),
		reclaimAt:   slices.Clone(h.reclaimAt),
		scopes:      slices.Clone(h.scopes),
	}
}

// blockingTool is a handler that tells the test it was entered, then waits
// to be let go, as a worker that stalls inside a tool would.
type blockingTool struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingTool() *blockingTool {
	return &blockingTool{entered: make(chan struct{}, 4), release: make(chan struct{})}
}

func (b *blockingTool) handler(ctx context.Context, _ cortex.Invocation) (string, error) {
	b.calls.Add(1)
	b.entered <- struct{}{}
	select {
	case <-b.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return "stale-result", nil
}

func (b *blockingTool) letGo() { b.once.Do(func() { close(b.release) }) }

func (b *blockingTool) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the tool to be entered")
	}
}

// countingTool returns its result and counts its calls.
type countingTool struct {
	calls  atomic.Int32
	result string
}

func (c *countingTool) handler(context.Context, cortex.Invocation) (string, error) {
	c.calls.Add(1)
	return c.result, nil
}

const reclaimTTL = 200 * time.Millisecond

// stall pauses the worker's heartbeat and waits until its lease has
// lapsed in the store, as a worker stuck in a GC pause or a dead process
// would leave it. Nothing sweeps on its own here: each test sweeps when it
// wants to.
func stall(t *testing.T, f *leaseFixture, runID id.AgentRunID) {
	t.Helper()
	engine.PauseLeaseRenewals(f.eng, true)
	require.Eventually(t, func() bool {
		r, err := f.store.GetRun(leaseScope(), runID)
		return err == nil && r.Lease != nil && time.Now().After(r.Lease.ExpiresAt)
	}, 5*time.Second, 5*time.Millisecond, "the stalled worker's lease never lapsed")
}

// reclaimer is a second replica on the same store.
func reclaimer(t *testing.T, f *leaseFixture, model llm.Client, hooks *reclaimHooks, opts ...engine.Option) *engine.Engine {
	t.Helper()
	base := []engine.Option{engine.WithStore(f.store), engine.WithLLM(model), engine.WithExtension(hooks), engine.WithWorkerID("replica-b"), engine.WithRunLease(time.Minute)}
	eng, err := engine.New(append(base, opts...)...)
	require.NoError(t, err)
	return eng
}

func requireUniqueStepIndexes(t *testing.T, f *leaseFixture, runID id.AgentRunID) {
	t.Helper()
	steps, err := f.store.ListSteps(leaseScope(), runID)
	require.NoError(t, err)
	seen := map[int]bool{}
	for _, s := range steps {
		require.False(t, seen[s.Index], "two step rows for index %d", s.Index)
		seen[s.Index] = true
	}
}

// storedRunningRun writes a running run under lease straight through the
// store, as a worker that died at once would leave it.
func storedRunningRun(t *testing.T, f *leaseFixture, lease run.Lease) *run.Run {
	t.Helper()
	ag, err := f.store.GetByName(leaseScope(), "assistant")
	require.NoError(t, err)
	now := time.Now().UTC()
	r := &run.Run{Entity: cortex.NewEntity(), ID: id.NewAgentRunID(), AgentID: ag.ID, State: run.StateRunning, Input: "work", StartedAt: &now, Lease: &lease}
	require.NoError(t, f.store.CreateRun(leaseScope(), r))
	return r
}

func TestSweeperMarksLapsedRunReclaimable(t *testing.T) {
	f := newLeaseFixture(t, newLeaseLLM())
	r := storedRunningRun(t, f, run.Lease{Owner: "dead", ExpiresAt: time.Now().UTC().Add(-time.Second)})
	hooks := newReclaimHooks()
	b := reclaimer(t, f, &reclaimLLM{}, hooks)

	engine.SweepOnce(b, context.Background())

	got, err := f.store.GetRun(leaseScope(), r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateReclaimable, got.State)
	require.Equal(t, 1, got.Reclaims)
	seen := hooks.snapshot()
	require.Len(t, seen.reclaimable, 1)
	require.Equal(t, "dead", seen.reclaimable[0].PreviousOwner)
	require.Equal(t, 1, seen.reclaimable[0].Reclaims)
	require.False(t, seen.reclaimable[0].HandOff)
	require.False(t, seen.reclaimable[0].Renotified)
	require.Equal(t, got.Scope.Canonical(), seen.scopes[0].Canonical())

	// A second sweep finds nothing: the run is no longer running.
	engine.SweepOnce(b, context.Background())
	require.Len(t, hooks.snapshot().reclaimable, 1)
}

func TestContinueReclaimedRunRerunsInterruptedModelCall(t *testing.T) {
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-a", Name: "a", Arguments: `{}`}))
	model.hold[1] = true
	a := &countingTool{result: "a-result"}
	f := newLeaseFixture(t, model, engine.WithRunLease(reclaimTTL),
		engine.WithTool(llm.Tool{Name: "a"}, a.handler), engine.WithToolRetrySafe("a"))
	done := f.start()
	model.waitEntered(t)
	r := f.onlyRun(t)
	stall(t, f, r.ID)

	hooks := newReclaimHooks()
	bModel := &reclaimLLM{}
	bTool := &countingTool{result: "a-again"}
	b := reclaimer(t, f, bModel, hooks, engine.WithTool(llm.Tool{Name: "a"}, bTool.handler), engine.WithToolRetrySafe("a"))
	engine.SweepOnce(b, context.Background())

	// The stalled worker wakes up: its model call returns, and it learns at
	// its terminal write that the run is no longer its own.
	model.letGo()
	out := waitOutcome(t, done)
	require.ErrorIs(t, out.err, cortex.ErrRunLeaseLost)

	continued, err := b.ContinueRun(leaseScope(), r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, continued.State)
	require.Equal(t, 1, bModel.calls(), "the reclaimed run made the interrupted model call once more")
	require.Zero(t, bTool.calls.Load(), "a call recorded as done ran again")
	require.Equal(t, int32(1), a.calls.Load())
	requireUniqueStepIndexes(t, f, r.ID)
}

func TestContinueReclaimedRunReplaysOnlyMissingCalls(t *testing.T) {
	model := newLeaseLLM(toolCalls(
		llm.ToolCall{ID: "call-a", Name: "a", Arguments: `{}`},
		llm.ToolCall{ID: "call-b", Name: "b", Arguments: `{}`},
	))
	a := &countingTool{result: "a-result"}
	stuck := newBlockingTool()
	f := newLeaseFixture(t, model, engine.WithRunLease(reclaimTTL),
		engine.WithTool(llm.Tool{Name: "a"}, a.handler),
		engine.WithTool(llm.Tool{Name: "b"}, stuck.handler),
		engine.WithToolRetrySafe("a", "b"))
	t.Cleanup(stuck.letGo)
	f.start()
	stuck.waitEntered(t)
	r := f.onlyRun(t)
	stall(t, f, r.ID)

	hooks := newReclaimHooks()
	bModel := &reclaimLLM{}
	bA, bB := &countingTool{result: "a-again"}, &countingTool{result: "b-result"}
	b := reclaimer(t, f, bModel, hooks,
		engine.WithTool(llm.Tool{Name: "a"}, bA.handler),
		engine.WithTool(llm.Tool{Name: "b"}, bB.handler),
		engine.WithToolRetrySafe("a", "b"))
	engine.SweepOnce(b, context.Background())

	continued, err := b.ContinueRun(leaseScope(), r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, continued.State)
	require.Zero(t, bA.calls.Load(), "a's result was saved, so a must not run again")
	require.Equal(t, int32(1), bB.calls.Load())
	msgs := bModel.firstRequest(t).Messages
	var tools []llm.Message
	for _, m := range msgs {
		if m.Role == "tool" {
			tools = append(tools, m)
		}
	}
	require.Len(t, tools, 2)
	require.Equal(t, "call-a", tools[0].ToolCallID)
	require.Equal(t, "a-result", tools[0].Content)
	require.Equal(t, "call-b", tools[1].ToolCallID)
	require.Equal(t, "b-result", tools[1].Content)
}

func TestContinueReclaimedRunFailsUncertainDispatch(t *testing.T) {
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-b", Name: "b", Arguments: `{}`}))
	stuck := newBlockingTool()
	f := newLeaseFixture(t, model, engine.WithRunLease(reclaimTTL), engine.WithTool(llm.Tool{Name: "b"}, stuck.handler))
	t.Cleanup(stuck.letGo)
	f.start()
	stuck.waitEntered(t)
	r := f.onlyRun(t)
	stall(t, f, r.ID)
	saved, err := f.store.GetProgress(leaseScope(), r.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"call-b"}, startedIDs(saved.Started))

	hooks := newReclaimHooks()
	bModel := &reclaimLLM{}
	bB := &countingTool{result: "b-again"}
	b := reclaimer(t, f, bModel, hooks, engine.WithTool(llm.Tool{Name: "b"}, bB.handler))
	engine.SweepOnce(b, context.Background())

	_, err = b.ContinueRun(leaseScope(), r.ID)
	require.ErrorIs(t, err, cortex.ErrEffectUncertain)
	require.Contains(t, err.Error(), `"b"`)
	require.Zero(t, bB.calls.Load(), "an uncertain call was dispatched again")
	require.Zero(t, bModel.calls())
	got, err := f.store.GetRun(leaseScope(), r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateIncomplete, got.State)
	require.Equal(t, run.StopToolError, got.StopReason)
	require.Equal(t, cortex.ErrEffectUncertain.Error(), got.Error)
	require.Equal(t, "b", got.Metadata["uncertain_tool"])
	require.Equal(t, "call-b", got.Metadata["uncertain_call_id"])
	require.Equal(t, []string{"b"}, hooks.snapshot().toolFailed)
}

func TestContinueReclaimedRunRedispatchesRetrySafe(t *testing.T) {
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-a", Name: "a", Arguments: `{}`}))
	stuck := newBlockingTool()
	f := newLeaseFixture(t, model, engine.WithRunLease(reclaimTTL),
		engine.WithTool(llm.Tool{Name: "a"}, stuck.handler), engine.WithToolRetrySafe("a"))
	t.Cleanup(stuck.letGo)
	f.start()
	stuck.waitEntered(t)
	r := f.onlyRun(t)
	stall(t, f, r.ID)

	hooks := newReclaimHooks()
	bModel := &reclaimLLM{}
	bA := &countingTool{result: "a-result"}
	b := reclaimer(t, f, bModel, hooks, engine.WithTool(llm.Tool{Name: "a"}, bA.handler), engine.WithToolRetrySafe("a"))
	engine.SweepOnce(b, context.Background())

	continued, err := b.ContinueRun(leaseScope(), r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, continued.State)
	require.Equal(t, int32(1), bA.calls.Load())
	require.Equal(t, 1, bModel.calls())
	require.Equal(t, 1, hooks.snapshot().completed)
}

// approveC escalates every call to c, and lets everything else through.
type approveC struct{}

func (approveC) Visible(_ context.Context, _ cortex.Subject, tools []llm.Tool) []llm.Tool {
	return tools
}

func (approveC) Authorize(_ context.Context, _ cortex.Subject, call llm.ToolCall) error {
	if call.Name == "c" {
		return cortex.ApprovalRequired{Reason: "c changes things", TTL: time.Hour}
	}
	return nil
}

func TestReclaimedPendingApprovalSuspendsAgain(t *testing.T) {
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-c", Name: "c", Arguments: `{}`}))
	stuck := newBlockingTool()
	f := newLeaseFixture(t, model, engine.WithRunLease(reclaimTTL),
		engine.WithTool(llm.Tool{Name: "c"}, stuck.handler), engine.WithToolRetrySafe("c"))
	t.Cleanup(stuck.letGo)
	f.start()
	stuck.waitEntered(t)
	r := f.onlyRun(t)
	stall(t, f, r.ID)

	hooks := newReclaimHooks()
	bModel := &reclaimLLM{}
	bC := &countingTool{result: "c-result"}
	b := reclaimer(t, f, bModel, hooks,
		engine.WithTool(llm.Tool{Name: "c"}, bC.handler), engine.WithToolRetrySafe("c"),
		engine.WithToolAuthorizer(approveC{}), engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[leasePrincipal]()))
	engine.SweepOnce(b, context.Background())

	paused, err := b.ContinueRun(leaseScope(), r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, paused.State)
	require.Zero(t, bC.calls.Load(), "a call that needs approval ran without one")
	require.Zero(t, bModel.calls())
	cps, err := f.store.ListPending(leaseScope(), &checkpoint.ListFilter{RunID: r.ID.String()})
	require.NoError(t, err)
	require.Len(t, cps, 1)
	seen := hooks.snapshot()
	require.Len(t, seen.suspended, 1)
	require.Equal(t, suspension.ReasonApproval, seen.suspended[0].Reason)
	require.Equal(t, cps[0].ID, seen.suspended[0].CheckpointID)
	require.Equal(t, []string{"call-c"}, startedIDs(seen.suspended[0].Pending))
}

func TestContinueRunningLapsedRunReclaimsInline(t *testing.T) {
	model := newLeaseLLM()
	model.hold[0] = true
	f := newLeaseFixture(t, model, engine.WithRunLease(reclaimTTL))
	done := f.start()
	model.waitEntered(t)
	r := f.onlyRun(t)
	stall(t, f, r.ID)

	hooks := newReclaimHooks()
	bModel := &reclaimLLM{}
	b := reclaimer(t, f, bModel, hooks)
	continued, err := b.ContinueRun(leaseScope(), r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, continued.State)
	require.Equal(t, 1, continued.Reclaims)
	require.Equal(t, 1, bModel.calls())

	model.letGo()
	require.ErrorIs(t, waitOutcome(t, done).err, cortex.ErrRunLeaseLost)

	// A live lease still refuses.
	live := storedRunningRun(t, f, run.Lease{Owner: "alive", ExpiresAt: time.Now().UTC().Add(time.Hour)})
	_, err = b.ContinueRun(leaseScope(), live.ID)
	require.ErrorIs(t, err, cortex.ErrRunLeased)
}

// reclaimNTimes takes a stored run through n lapsed leases.
func reclaimNTimes(t *testing.T, f *leaseFixture, runID id.AgentRunID, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if i > 0 {
			_, err := f.store.ClaimContinuation(leaseScope(), runID, run.Lease{Owner: "dead", ExpiresAt: time.Now().UTC().Add(-time.Second)})
			require.NoError(t, err)
		}
		_, err := f.store.MarkReclaimable(leaseScope(), runID, time.Now().UTC(), "")
		require.NoError(t, err)
	}
}

func TestReclaimLimitFailsPoisonRun(t *testing.T) {
	f := newLeaseFixture(t, newLeaseLLM())
	r := storedRunningRun(t, f, run.Lease{Owner: "dead", ExpiresAt: time.Now().UTC().Add(-time.Second)})
	reclaimNTimes(t, f, r.ID, 4)

	hooks := newReclaimHooks()
	bModel := &reclaimLLM{}
	b := reclaimer(t, f, bModel, hooks, engine.WithReclaimLimit(3))
	_, err := b.ContinueRun(leaseScope(), r.ID)
	require.ErrorIs(t, err, cortex.ErrReclaimLimit)
	require.Zero(t, bModel.calls())
	got, err := f.store.GetRun(leaseScope(), r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateFailed, got.State)
	require.Contains(t, got.Error, cortex.ErrReclaimLimit.Error())
	require.Equal(t, 1, hooks.snapshot().failed)
}

func TestNoProgressFailsRunClearly(t *testing.T) {
	f := newLeaseFixture(t, newLeaseLLM())
	r := storedRunningRun(t, f, run.Lease{Owner: "dead", ExpiresAt: time.Now().UTC().Add(-time.Second)})
	reclaimNTimes(t, f, r.ID, 1)

	hooks := newReclaimHooks()
	bModel := &reclaimLLM{}
	b := reclaimer(t, f, bModel, hooks)
	_, err := b.ContinueRun(leaseScope(), r.ID)
	require.ErrorIs(t, err, cortex.ErrRunNotRecoverable)
	require.Zero(t, bModel.calls())
	got, err := f.store.GetRun(leaseScope(), r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateFailed, got.State)
	require.Contains(t, got.Error, cortex.ErrRunNotRecoverable.Error())
}

func TestStaleWorkerCannotFinishReclaimedRun(t *testing.T) {
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-a", Name: "a", Arguments: `{}`}))
	stuck := newBlockingTool()
	f := newLeaseFixture(t, model, engine.WithRunLease(reclaimTTL),
		engine.WithTool(llm.Tool{Name: "a"}, stuck.handler), engine.WithToolRetrySafe("a"))
	t.Cleanup(stuck.letGo)
	done := f.start()
	stuck.waitEntered(t)
	r := f.onlyRun(t)
	stall(t, f, r.ID)

	hooks := newReclaimHooks()
	bModel := &reclaimLLM{}
	bA := &countingTool{result: "a-result"}
	b := reclaimer(t, f, bModel, hooks, engine.WithTool(llm.Tool{Name: "a"}, bA.handler), engine.WithToolRetrySafe("a"))
	engine.SweepOnce(b, context.Background())
	continued, err := b.ContinueRun(leaseScope(), r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, continued.State)

	// The stale worker's tool returns at last.
	stuck.letGo()
	out := waitOutcome(t, done)
	require.ErrorIs(t, out.err, cortex.ErrRunLeaseLost)

	got, err := f.store.GetRun(leaseScope(), r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, got.State)
	require.Equal(t, continued.Output, got.Output)
	staleCompleted, _, _, _ := f.hooks.counts()
	require.Zero(t, staleCompleted, "the stale worker announced a completion")
	require.Equal(t, 1, hooks.snapshot().completed)
}

func TestSweeperRenotifiesStaleContinuableRuns(t *testing.T) {
	f := newLeaseFixture(t, newLeaseLLM())
	// A ready run nobody was ever told about: the store moved it, and the
	// process that should have fired RunReady died before it could.
	paused := storedRunningRun(t, f, run.Lease{Owner: "dead", ExpiresAt: time.Now().UTC().Add(-time.Second)})
	susp := &suspension.Suspension{Entity: cortex.NewEntity(), ID: id.NewSuspensionID(), RunID: paused.ID, Reason: suspension.ReasonExternalTool,
		Pending: []suspension.PendingCall{{ID: "call-x", Name: "x", Arguments: "{}"}}, Cont: suspension.Continuation{Config: suspension.RunConfig{MaxSteps: 3}}}
	require.NoError(t, f.store.CreateSuspension(leaseScope(), susp))
	paused.State = run.StatePaused
	require.NoError(t, f.store.UpdateRun(leaseScope(), paused))
	_, err := f.store.MarkReady(leaseScope(), paused.ID, run.Readiness{Source: run.ReadyExternalResults, SuspensionID: susp.ID, Results: []byte(`[{"tool_call_id":"call-x","content":"x"}]`), At: time.Now().UTC()})
	require.NoError(t, err)
	// And a reclaimable one, the same.
	lapsed := storedRunningRun(t, f, run.Lease{Owner: "dead", ExpiresAt: time.Now().UTC().Add(-time.Second)})
	reclaimNTimes(t, f, lapsed.ID, 1)

	hooks := newReclaimHooks()
	b := reclaimer(t, f, &reclaimLLM{}, hooks,
		engine.WithReadyRenotify(50*time.Millisecond), engine.WithSuspensionSweepInterval(10*time.Millisecond))
	require.NoError(t, b.Start(context.Background()))
	t.Cleanup(func() { _ = b.Stop(context.Background()) })

	require.Eventually(t, func() bool {
		seen := hooks.snapshot()
		return len(seen.ready) >= 2 && len(seen.reclaimable) >= 1
	}, 2*time.Second, 5*time.Millisecond, "the sweeper did not re-announce the waiting runs")
	seen := hooks.snapshot()
	require.True(t, seen.ready[0].Renotified)
	require.Equal(t, run.ReadyExternalResults, seen.ready[0].Source)
	require.True(t, seen.reclaimable[0].Renotified)
	require.Equal(t, 1, seen.reclaimable[0].Reclaims)
	// At most once per window: consecutive re-notices of the same run are
	// at least the window apart (less a little for the hook's own delay).
	for i := 1; i < len(seen.readyAt); i++ {
		require.GreaterOrEqual(t, seen.readyAt[i].Sub(seen.readyAt[i-1]), 40*time.Millisecond, "re-notified a ready run twice inside one window")
	}
	for i := 1; i < len(seen.reclaimAt); i++ {
		require.GreaterOrEqual(t, seen.reclaimAt[i].Sub(seen.reclaimAt[i-1]), 40*time.Millisecond, "re-notified a reclaimable run twice inside one window")
	}
}

func TestReclaimMetrics(t *testing.T) {
	model := newLeaseLLM()
	model.hold[0] = true
	f := newLeaseFixture(t, model, engine.WithRunLease(reclaimTTL))
	done := f.start()
	model.waitEntered(t)
	r := f.onlyRun(t)
	stall(t, f, r.ID)

	sink := telemetrytest.NewSink()
	b := reclaimer(t, f, &reclaimLLM{}, newReclaimHooks(), engine.WithMetrics(sink))
	engine.SweepOnce(b, context.Background())
	require.Equal(t, 1.0, sink.Value("cortex_runs_reclaimed_total", map[string]string{"result": "reclaimed"}))

	_, err := b.ContinueRun(leaseScope(), r.ID)
	require.NoError(t, err)
	require.Equal(t, 1.0, sink.Value("cortex_runs_reclaimed_total", map[string]string{"result": "continued"}))

	model.letGo()
	require.Error(t, waitOutcome(t, done).err)

	// A run with nothing to continue from counts as unrecoverable.
	lost := storedRunningRun(t, f, run.Lease{Owner: "dead", ExpiresAt: time.Now().UTC().Add(-time.Second)})
	reclaimNTimes(t, f, lost.ID, 1)
	_, err = b.ContinueRun(leaseScope(), lost.ID)
	require.True(t, errors.Is(err, cortex.ErrRunNotRecoverable))
	require.Equal(t, 1.0, sink.Value("cortex_runs_reclaimed_total", map[string]string{"result": "unrecoverable"}))

	// One reclaimed too often counts as limit.
	poison := storedRunningRun(t, f, run.Lease{Owner: "dead", ExpiresAt: time.Now().UTC().Add(-time.Second)})
	reclaimNTimes(t, f, poison.ID, 4)
	_, err = b.ContinueRun(leaseScope(), poison.ID)
	require.ErrorIs(t, err, cortex.ErrReclaimLimit)
	require.Equal(t, 1.0, sink.Value("cortex_runs_reclaimed_total", map[string]string{"result": "limit"}))
}

func TestReclaimMetricsCountUncertain(t *testing.T) {
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-b", Name: "b", Arguments: `{}`}))
	stuck := newBlockingTool()
	f := newLeaseFixture(t, model, engine.WithRunLease(reclaimTTL), engine.WithTool(llm.Tool{Name: "b"}, stuck.handler))
	t.Cleanup(stuck.letGo)
	f.start()
	stuck.waitEntered(t)
	r := f.onlyRun(t)
	stall(t, f, r.ID)

	sink := telemetrytest.NewSink()
	b := reclaimer(t, f, &reclaimLLM{}, newReclaimHooks(), engine.WithMetrics(sink), engine.WithTool(llm.Tool{Name: "b"}, (&countingTool{}).handler))
	engine.SweepOnce(b, context.Background())
	_, err := b.ContinueRun(leaseScope(), r.ID)
	require.ErrorIs(t, err, cortex.ErrEffectUncertain)
	require.Equal(t, 1.0, sink.Value("cortex_runs_reclaimed_total", map[string]string{"result": "uncertain"}))
}

// principalLog is an authorizer that allows everything and records the
// principal each check was asked about.
type principalLog struct {
	mu   sync.Mutex
	seen []any
}

func (p *principalLog) record(s cortex.Subject) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, s.Principal)
}

func (p *principalLog) Visible(_ context.Context, s cortex.Subject, tools []llm.Tool) []llm.Tool {
	p.record(s)
	return tools
}

func (p *principalLog) Authorize(_ context.Context, s cortex.Subject, _ llm.ToolCall) error {
	p.record(s)
	return nil
}

func (p *principalLog) principals() []any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.seen)
}

// revokingCodec is the JSON codec, except that it reports one user's
// identity as revoked, as a host does once that member is removed.
type revokingCodec struct {
	cortex.PrincipalCodec
	revoked string
}

func (c revokingCodec) DecodePrincipal(ctx context.Context, data []byte) (any, error) {
	p, err := c.PrincipalCodec.DecodePrincipal(ctx, data)
	if err == nil && p.(leasePrincipal).User == c.revoked {
		return nil, fmt.Errorf("member %s was removed: %w", c.revoked, cortex.ErrPrincipalRevoked)
	}
	return p, err
}

// stallUnderPrincipal starts a run as alice whose worker stalls inside the
// retry-safe tool "b", and waits until its lease has lapsed.
func stallUnderPrincipal(t *testing.T) (*leaseFixture, *run.Run) {
	t.Helper()
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-b", Name: "b", Arguments: `{}`}))
	stuck := newBlockingTool()
	f := newLeaseFixture(t, model, engine.WithRunLease(reclaimTTL),
		engine.WithTool(llm.Tool{Name: "b"}, stuck.handler), engine.WithToolRetrySafe("b"),
		engine.WithToolAuthorizer(&principalLog{}),
		engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[leasePrincipal]()))
	t.Cleanup(stuck.letGo)
	f.runs.Add(1)
	go func() {
		defer f.runs.Done()
		_, _ = f.eng.RunAgent(cortex.WithPrincipal(leaseScope(), leasePrincipal{User: "alice"}), "assistant", "work", nil)
	}()
	stuck.waitEntered(t)
	r := f.onlyRun(t)
	stall(t, f, r.ID)
	return f, r
}

// A reclaimed run is authorized as the caller who started it, never as the
// job that continues it on another replica: the replayed call and every
// call after it are asked about alice.
func TestReclaimedRunKeepsItsOriginalPrincipal(t *testing.T) {
	f, r := stallUnderPrincipal(t)

	authB := &principalLog{}
	bModel := &reclaimLLM{responses: []*llm.Response{toolCalls(llm.ToolCall{ID: "call-b2", Name: "b", Arguments: `{}`})}}
	bB := &countingTool{result: "b-again"}
	b := reclaimer(t, f, bModel, newReclaimHooks(),
		engine.WithTool(llm.Tool{Name: "b"}, bB.handler), engine.WithToolRetrySafe("b"),
		engine.WithToolAuthorizer(authB),
		engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[leasePrincipal]()))
	engine.SweepOnce(b, context.Background())

	job := cortex.WithPrincipal(leaseScope(), leasePrincipal{User: "job"})
	continued, err := b.ContinueRun(job, r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, continued.State)
	require.Equal(t, int32(2), bB.calls.Load(), "the reclaimer did not run both the replayed call and the next one")

	seen := authB.principals()
	require.NotEmpty(t, seen, "the reclaimer never consulted its authorizer")
	for _, p := range seen {
		require.Equal(t, leasePrincipal{User: "alice"}, p, "a reclaimed run was authorized as someone else")
	}
}

// A reclaimed run whose caller has been revoked since is failed with
// cortex.ErrPrincipalRevoked before the reclaimer makes a model call or
// runs a tool for it.
func TestReclaimRefusesARevokedPrincipal(t *testing.T) {
	f, r := stallUnderPrincipal(t)

	authB := &principalLog{}
	bModel := &reclaimLLM{}
	bB := &countingTool{result: "b-again"}
	hooks := newReclaimHooks()
	b := reclaimer(t, f, bModel, hooks,
		engine.WithTool(llm.Tool{Name: "b"}, bB.handler), engine.WithToolRetrySafe("b"),
		engine.WithToolAuthorizer(authB),
		engine.WithPrincipalCodec(revokingCodec{PrincipalCodec: cortex.JSONPrincipalCodec[leasePrincipal](), revoked: "alice"}))
	engine.SweepOnce(b, context.Background())

	ended, err := b.ContinueRun(cortex.WithPrincipal(leaseScope(), leasePrincipal{User: "job"}), r.ID)
	require.ErrorIs(t, err, cortex.ErrPrincipalRevoked)
	require.NotNil(t, ended)
	require.Equal(t, run.StateFailed, ended.State)
	stored, err := f.store.GetRun(leaseScope(), r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateFailed, stored.State)
	require.Zero(t, bModel.calls(), "the reclaimer called the model for a revoked caller")
	require.Zero(t, bB.calls.Load(), "the reclaimer ran a tool for a revoked caller")
	require.Empty(t, authB.principals(), "the reclaimer asked its authorizer about a revoked run")
	require.Equal(t, 1, hooks.snapshot().failed)
}
