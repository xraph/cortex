package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	sqlitestore "github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/suspension"
)

// leaseLLM answers each Complete with the next scripted response, and
// can hold a call until the test lets it go. A held call honours its ctx,
// so a run whose context is cancelled gets its call back with the error.
type leaseLLM struct {
	mu        sync.Mutex
	responses []*llm.Response
	calls     int
	// hold makes the call with this index (0-based) wait on release.
	hold    map[int]bool
	entered chan struct{}
	release chan struct{}
}

func newLeaseLLM(responses ...*llm.Response) *leaseLLM {
	return &leaseLLM{responses: responses, hold: map[int]bool{}, entered: make(chan struct{}, 16), release: make(chan struct{})}
}

func (m *leaseLLM) Complete(ctx context.Context, _ *llm.Request) (*llm.Response, error) {
	m.mu.Lock()
	n := m.calls
	m.calls++
	held := m.hold[n]
	var resp *llm.Response
	if n < len(m.responses) {
		resp = m.responses[n]
	} else {
		resp = &llm.Response{Content: "done", FinishReason: "stop"}
	}
	m.mu.Unlock()
	if held {
		m.entered <- struct{}{}
		select {
		case <-m.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return resp, nil
}

func (m *leaseLLM) CompleteStream(ctx context.Context, req *llm.Request) (llm.Stream, error) {
	resp, err := m.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	return &admissionStream{chunk: &llm.Chunk{Content: resp.Content, FinishReason: resp.FinishReason, ToolCalls: resp.ToolCalls}}, nil
}

func (m *leaseLLM) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *leaseLLM) letGo() {
	defer func() { _ = recover() }()
	close(m.release)
}

func (m *leaseLLM) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-m.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the model call")
	}
}

func toolCalls(calls ...llm.ToolCall) *llm.Response {
	return &llm.Response{ToolCalls: calls, FinishReason: "tool_calls"}
}

// progressSpy is a SQLite store that keeps a copy of every snapshot
// saved, so a test can see each boundary rather than only the last.
type progressSpy struct {
	*sqlitestore.Store
	mu    sync.Mutex
	saved []suspension.Progress
	holds atomic.Int32
}

func (s *progressSpy) SaveProgress(ctx context.Context, p *suspension.Progress) error {
	if err := s.Store.SaveProgress(ctx, p); err != nil {
		return err
	}
	copied := *p
	copied.Started = slices.Clone(p.Started)
	copied.Done = slices.Clone(p.Done)
	copied.Cont.Messages = slices.Clone(p.Cont.Messages)
	s.mu.Lock()
	s.saved = append(s.saved, copied)
	s.mu.Unlock()
	return nil
}

func (s *progressSpy) HoldLease(ctx context.Context, runID id.AgentRunID, lease run.Lease) error {
	s.holds.Add(1)
	return s.Store.HoldLease(ctx, runID, lease)
}

func (s *progressSpy) snapshots() []suspension.Progress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.saved)
}

// lifecycleRecorder records the terminal hooks and every RunReclaimable.
type lifecycleRecorder struct {
	mu          sync.Mutex
	completed   int
	failed      int
	terminated  int
	reclaimable []run.ReclaimNotice
	scopes      []cortex.Scope
}

func (*lifecycleRecorder) Name() string { return "lifecycle-recorder" }

func (r *lifecycleRecorder) OnRunCompleted(context.Context, id.AgentID, id.AgentRunID, string, time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.completed++
	return nil
}

func (r *lifecycleRecorder) OnRunFailed(context.Context, id.AgentID, id.AgentRunID, error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed++
	return nil
}

func (r *lifecycleRecorder) OnRunTerminated(context.Context, id.AgentID, id.AgentRunID, run.TerminalSummary) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.terminated++
	return nil
}

func (r *lifecycleRecorder) OnRunReclaimable(ctx context.Context, _ id.AgentID, _ id.AgentRunID, n run.ReclaimNotice) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reclaimable = append(r.reclaimable, n)
	r.scopes = append(r.scopes, cortex.ScopeFromContext(ctx))
	return nil
}

func (r *lifecycleRecorder) counts() (completed, failed, terminated int, reclaimable []run.ReclaimNotice) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.completed, r.failed, r.terminated, slices.Clone(r.reclaimable)
}

func leaseScope() context.Context {
	return cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_lease"}}})
}

type leaseFixture struct {
	eng   *engine.Engine
	store *progressSpy
	model *leaseLLM
	hooks *lifecycleRecorder
	runs  sync.WaitGroup
}

func newLeaseFixture(t *testing.T, model *leaseLLM, opts ...engine.Option) *leaseFixture {
	t.Helper()
	st := &progressSpy{Store: openSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "lease.db")+"?_pragma=busy_timeout(10000)")}
	f := &leaseFixture{store: st, model: model, hooks: &lifecycleRecorder{}}
	t.Cleanup(func() {
		model.letGo()
		f.runs.Wait()
	})
	eng, err := engine.New(append([]engine.Option{engine.WithStore(st), engine.WithLLM(model), engine.WithExtension(f.hooks)}, opts...)...)
	require.NoError(t, err)
	require.NoError(t, eng.CreateAgent(leaseScope(), &agent.Config{ID: id.NewAgentID(), Name: "assistant", MaxSteps: 5}))
	f.eng = eng
	return f
}

func (f *leaseFixture) start() <-chan runOutcome {
	done := make(chan runOutcome, 1)
	f.runs.Add(1)
	go func() {
		defer f.runs.Done()
		r, err := f.eng.RunAgent(leaseScope(), "assistant", "work", nil)
		done <- runOutcome{run: r, err: err}
	}()
	return done
}

func (f *leaseFixture) onlyRun(t *testing.T) *run.Run {
	t.Helper()
	rows, err := f.eng.ListRuns(leaseScope(), &run.ListFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	return rows[0]
}

func TestRunHoldsLeaseWhileExecuting(t *testing.T) {
	model := newLeaseLLM()
	model.hold[0] = true
	f := newLeaseFixture(t, model, engine.WithRunLease(300*time.Millisecond))
	done := f.start()
	model.waitEntered(t)

	first := f.onlyRun(t)
	require.NotNil(t, first.Lease, "a running run carries its worker's lease")
	require.Equal(t, f.eng.WorkerID(), first.Lease.Owner)
	time.Sleep(200 * time.Millisecond)
	second := f.onlyRun(t)
	require.NotNil(t, second.Lease)
	require.True(t, second.Lease.ExpiresAt.After(first.Lease.ExpiresAt), "the lease was not renewed: %s then %s", first.Lease.ExpiresAt, second.Lease.ExpiresAt)

	model.letGo()
	out := waitOutcome(t, done)
	require.NoError(t, out.err)
	finished := f.onlyRun(t)
	require.Equal(t, run.StateCompleted, finished.State)
	require.Nil(t, finished.Lease)
}

func TestLeaseLostStopsWithoutTerminalWrite(t *testing.T) {
	model := newLeaseLLM()
	model.hold[0] = true
	f := newLeaseFixture(t, model, engine.WithRunLease(300*time.Millisecond))
	done := f.start()
	model.waitEntered(t)

	r := f.onlyRun(t)
	before, err := f.store.GetProgress(leaseScope(), r.ID)
	require.NoError(t, err)
	// Another replica's sweeper takes the run, as it would after a stall.
	_, err = f.store.MarkReclaimable(leaseScope(), r.ID, time.Now().Add(time.Hour), "")
	require.NoError(t, err)

	out := waitOutcome(t, done)
	require.ErrorIs(t, out.err, cortex.ErrRunLeaseLost)
	require.Equal(t, run.StateReclaimable, f.onlyRun(t).State)
	completed, failed, terminated, _ := f.hooks.counts()
	require.Zero(t, completed)
	require.Zero(t, failed)
	require.Zero(t, terminated)
	after, err := f.store.GetProgress(leaseScope(), r.ID)
	require.NoError(t, err)
	require.Equal(t, before.Cont.StepIndex, after.Cont.StepIndex)
	require.Equal(t, before.UpdatedAt, after.UpdatedAt, "the stale worker saved progress after losing its lease")
	require.Equal(t, f.eng.WorkerID(), after.Owner)
}

func TestProgressSavedAtStepBoundaries(t *testing.T) {
	model := newLeaseLLM(toolCalls(
		llm.ToolCall{ID: "call-a", Name: "a", Arguments: `{}`},
		llm.ToolCall{ID: "call-b", Name: "b", Arguments: `{"x":1}`},
	))
	var seenByB *suspension.Progress
	var store *progressSpy
	handlerB := func(ctx context.Context, inv cortex.Invocation) (string, error) {
		p, err := store.GetProgress(ctx, inv.RunID)
		if err != nil {
			return "", err
		}
		seenByB = p
		return "b-result", nil
	}
	f := newLeaseFixture(t, model,
		engine.WithTool(llm.Tool{Name: "a", Description: "reads"}, func(context.Context, cortex.Invocation) (string, error) { return "a-result", nil }),
		engine.WithTool(llm.Tool{Name: "b", Description: "writes"}, handlerB),
		engine.WithToolRetrySafe("a"),
	)
	store = f.store
	r, err := f.eng.RunAgent(leaseScope(), "assistant", "work", nil)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, r.State)

	require.NotNil(t, seenByB, "b's handler did not run")
	require.Equal(t, []string{"call-b"}, startedIDs(seenByB.Started))
	require.Equal(t, []string{"call-a"}, doneIDs(seenByB.Done))
	require.Equal(t, `{"x":1}`, seenByB.Started[0].Arguments)

	saved := f.store.snapshots()
	require.NotEmpty(t, saved)
	require.Zero(t, saved[0].Cont.StepIndex, "the first snapshot is the run's step 0")
	var afterModel *suspension.Progress
	for i := range saved {
		if saved[i].Cont.StepIndex == 1 && len(saved[i].Started) == 0 && len(saved[i].Done) == 0 {
			afterModel = &saved[i]
			break
		}
	}
	require.NotNil(t, afterModel, "no snapshot after the model response")
	last := afterModel.Cont.Messages[len(afterModel.Cont.Messages)-1]
	require.Equal(t, "assistant", last.Role)
	require.Len(t, last.ToolCalls, 2)
	final := saved[len(saved)-1]
	require.Equal(t, []string{"call-a", "call-b"}, doneIDs(final.Done))
	require.Equal(t, "b-result", final.Done[1].Content)
	require.Equal(t, "completed", final.Done[1].Outcome)
	for _, p := range saved {
		require.NotContains(t, startedIDs(p.Started), "call-a", "a retry-safe call was recorded as started")
		require.Equal(t, f.eng.WorkerID(), p.Owner)
	}

	_, err = f.store.GetProgress(leaseScope(), r.ID)
	require.ErrorIs(t, err, cortex.ErrProgressNotFound)
}

func startedIDs(calls []suspension.PendingCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.ID)
	}
	return out
}

func doneIDs(results []suspension.ProgressResult) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.CallID)
	}
	return out
}

// approveEverything escalates every call to a person.
type approveEverything struct{}

func (approveEverything) Visible(_ context.Context, _ cortex.Subject, tools []llm.Tool) []llm.Tool {
	return tools
}

func (approveEverything) Authorize(context.Context, cortex.Subject, llm.ToolCall) error {
	return cortex.ErrRequiresApproval
}

type leasePrincipal struct {
	User string `json:"user"`
}

func TestPauseDeletesProgress(t *testing.T) {
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-1", Name: "deploy", Arguments: `{}`}))
	f := newLeaseFixture(t, model,
		engine.WithTool(llm.Tool{Name: "deploy", Description: "needs a person"}, func(context.Context, cortex.Invocation) (string, error) { return "ok", nil }),
		engine.WithToolAuthorizer(approveEverything{}),
		engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[leasePrincipal]()),
	)
	r, err := f.eng.RunAgent(cortex.WithPrincipal(leaseScope(), leasePrincipal{User: "alice"}), "assistant", "work", nil)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, r.State)
	require.NotEmpty(t, f.store.snapshots(), "the run saved no progress before it paused")
	_, err = f.store.GetProgress(leaseScope(), r.ID)
	require.ErrorIs(t, err, cortex.ErrProgressNotFound)
	require.Nil(t, f.onlyRun(t).Lease)
}

func TestRunIDOverrideIsIdempotent(t *testing.T) {
	model := newLeaseLLM()
	f := newLeaseFixture(t, model)
	runID := id.NewAgentRunID()
	first, err := f.eng.RunAgent(leaseScope(), "assistant", "work", &engine.RunOverrides{RunID: runID})
	require.NoError(t, err)
	require.Equal(t, runID, first.ID)
	require.Equal(t, 1, model.callCount())

	_, err = f.eng.RunAgent(leaseScope(), "assistant", "work", &engine.RunOverrides{RunID: runID})
	require.ErrorIs(t, err, cortex.ErrAlreadyExists)
	require.Equal(t, 1, model.callCount(), "the duplicate ran the model")
	count, err := f.eng.CountRuns(leaseScope(), &run.ListFilter{})
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	require.Equal(t, run.StateCompleted, f.onlyRun(t).State)
}

func TestStopHandsOffAtNextBoundary(t *testing.T) {
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-1", Name: "write", Arguments: `{}`}))
	model.hold[0] = true
	var dispatched atomic.Int32
	f := newLeaseFixture(t, model, engine.WithTool(llm.Tool{Name: "write", Description: "writes"}, func(context.Context, cortex.Invocation) (string, error) {
		dispatched.Add(1)
		return "written", nil
	}))
	require.NoError(t, f.eng.Start(context.Background()))
	done := f.start()
	model.waitEntered(t)

	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		stopped <- f.eng.Stop(ctx)
	}()
	// Stop is draining once new runs are refused. The probe's context is
	// already cancelled, so a probe admitted before the drain fails before
	// it can create a run.
	probe, cancelProbe := context.WithCancel(leaseScope())
	cancelProbe()
	require.Eventually(t, func() bool {
		_, err := f.eng.RunAgent(probe, "assistant", "late", nil)
		return errors.Is(err, cortex.ErrEngineStopping)
	}, 2*time.Second, 10*time.Millisecond)
	model.letGo()

	out := waitOutcome(t, done)
	require.ErrorIs(t, out.err, cortex.ErrRunInterrupted)
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the run handed off")
	}
	require.Zero(t, dispatched.Load(), "the run dispatched a tool while handing off")
	r := f.onlyRun(t)
	require.Equal(t, run.StateReclaimable, r.State)
	require.Zero(t, r.Reclaims)
	require.Nil(t, r.Lease)
	_, _, terminated, notices := f.hooks.counts()
	require.Zero(t, terminated)
	require.Len(t, notices, 1)
	require.True(t, notices[0].HandOff)
	require.Equal(t, f.eng.WorkerID(), notices[0].PreviousOwner)
	require.Zero(t, notices[0].Reclaims)
	p, err := f.store.GetProgress(leaseScope(), r.ID)
	require.NoError(t, err)
	require.Equal(t, 1, p.Cont.StepIndex)
	require.Empty(t, p.Started)
	require.Empty(t, p.Done)
}

func TestStopDeadlineCancelsBlockedRun(t *testing.T) {
	model := newLeaseLLM()
	model.hold[0] = true
	f := newLeaseFixture(t, model)
	require.NoError(t, f.eng.Start(context.Background()))
	done := f.start()
	model.waitEntered(t)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, f.eng.Stop(ctx), context.DeadlineExceeded)
	out := waitOutcome(t, done)
	require.ErrorIs(t, out.err, cortex.ErrRunInterrupted)
	require.Eventually(t, func() bool {
		return f.onlyRun(t).State == run.StateReclaimable
	}, 2*time.Second, 20*time.Millisecond)
	_, failed, terminated, notices := f.hooks.counts()
	require.Zero(t, failed)
	require.Zero(t, terminated)
	require.Len(t, notices, 1)
	require.True(t, notices[0].HandOff)
	require.NoError(t, f.eng.Stop(context.Background()))
}

func TestUnleasedRuns(t *testing.T) {
	t.Run("leases off", func(t *testing.T) {
		model := newLeaseLLM()
		model.hold[0] = true
		f := newLeaseFixture(t, model, engine.WithRunLease(0))
		done := f.start()
		model.waitEntered(t)
		require.Nil(t, f.onlyRun(t).Lease)
		model.letGo()
		require.NoError(t, waitOutcome(t, done).err)
		require.Empty(t, f.store.snapshots())
		require.Zero(t, f.store.holds.Load())
	})
	t.Run("task claim", func(t *testing.T) {
		fx, e, client, prepared, req, admitter := preparedTaskRuntimeFixture(t)
		require.NoError(t, engine.WithTaskRunAdmitter(admitter)(e))
		require.NoError(t, engine.WithTaskCallGuard(runtimeTaskGuard(allowRuntimeTaskCall))(e))
		client.before = func() { requireUnleased(fx.ctx, t, fx.storage, req.Run.ID) }
		r, err := e.RunPreparedTask(fx.ctx, prepared, req)
		require.NoError(t, err)
		require.Equal(t, 1, client.calls)
		requireUnleased(fx.ctx, t, fx.storage, r.ID)
	})
	t.Run("required verification", func(t *testing.T) {
		fx := newWorkerFixture(t)
		client := &requiredClient{}
		e, ag, overrides := requiredSetup(t, fx, client)
		var runID id.AgentRunID
		client.before = func() {
			rows, err := fx.storage.ListRuns(fx.ctx, &run.ListFilter{AgentID: ag.ID.String()})
			require.NoError(t, err)
			require.Len(t, rows, 1)
			runID = rows[0].ID
			requireUnleased(fx.ctx, t, fx.storage, runID)
		}
		prepared, err := e.PrepareAgent(fx.ctx, ag.ID, overrides)
		require.NoError(t, err)
		encoded, err := json.Marshal(prepared)
		require.NoError(t, err)
		var restored engine.PreparedAgent
		require.NoError(t, json.Unmarshal(encoded, &restored))
		_, err = e.RunPrepared(fx.ctx, &restored, "work", engine.PreparedRunOptions{})
		require.NoError(t, err)
		require.Equal(t, 1, client.calls)
		requireUnleased(fx.ctx, t, fx.storage, runID)
	})
}

func requireUnleased(ctx context.Context, t *testing.T, st *sqlitestore.Store, runID id.AgentRunID) {
	t.Helper()
	r, err := st.GetRun(ctx, runID)
	require.NoError(t, err)
	require.Nil(t, r.Lease)
	_, err = st.GetProgress(ctx, runID)
	require.ErrorIs(t, err, cortex.ErrProgressNotFound)
}
