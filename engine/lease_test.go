package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// failStarted fails that many saves that record a started call, with
	// an ordinary store error rather than a lost lease.
	failStarted atomic.Int32
	// failHolds fails that many HoldLease calls the same way.
	failHolds atomic.Int32
	// failEnds fails that many writes of a cancelled or failed run, as a
	// store that drops a terminal write would.
	failEnds atomic.Int32
	// afterClaim, when set, runs after a leased claim commits and before it
	// returns, as a worker that stalls or dies right after its claim would.
	afterClaim func()
}

func (s *progressSpy) ClaimSuspensionLeased(ctx context.Context, claim suspension.LeasedClaim) (*suspension.Suspension, error) {
	susp, err := s.Store.ClaimSuspensionLeased(ctx, claim)
	if err == nil && s.afterClaim != nil {
		s.afterClaim()
	}
	return susp, err
}

func (s *progressSpy) ClaimContinuationLeased(ctx context.Context, runID id.AgentRunID, lease run.Lease, first *suspension.Progress) (*run.Run, error) {
	r, err := s.Store.ClaimContinuationLeased(ctx, runID, lease, first)
	if err == nil && s.afterClaim != nil {
		s.afterClaim()
	}
	return r, err
}

var errStoreDown = errors.New("store unavailable")

func (s *progressSpy) SaveProgress(ctx context.Context, p *suspension.Progress) error {
	if len(p.Started) > 0 && s.failStarted.Add(-1) >= 0 {
		return errStoreDown
	}
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

func (s *progressSpy) UpdateRun(ctx context.Context, r *run.Run) error {
	if (r.State == run.StateCancelled || r.State == run.StateFailed) && s.failEnds.Add(-1) >= 0 {
		return errStoreDown
	}
	return s.Store.UpdateRun(ctx, r)
}

func (s *progressSpy) HoldLease(ctx context.Context, runID id.AgentRunID, lease run.Lease) error {
	s.holds.Add(1)
	if s.failHolds.Add(-1) >= 0 {
		return errStoreDown
	}
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

// newLeaseFixture builds an engine that leases its runs for testLease,
// since leasing is off by default. A test passes its own WithRunLease in
// opts to change that, the later option winning.
func newLeaseFixture(t *testing.T, model *leaseLLM, opts ...engine.Option) *leaseFixture {
	t.Helper()
	return buildLeaseFixture(t, model, append([]engine.Option{engine.WithRunLease(testLease)}, opts...))
}

// newDefaultFixture is newLeaseFixture on the engine's own lease setting,
// which is off.
func newDefaultFixture(t *testing.T, model *leaseLLM, opts ...engine.Option) *leaseFixture {
	t.Helper()
	return buildLeaseFixture(t, model, opts)
}

func buildLeaseFixture(t *testing.T, model *leaseLLM, opts []engine.Option) *leaseFixture {
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

// testLease is the lease ttl the lease tests turn leasing on with.
const testLease = 60 * time.Second

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

// With leasing off, the default, Stop lets a run that is executing finish,
// as v1.9.0 did. Nothing is handed off, no run is left reclaimable, and the
// run never holds a lease the lapsed sweep could find.
func TestDefaultStopDrainsWithoutHandOff(t *testing.T) {
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-1", Name: "write", Arguments: `{}`}))
	model.hold[0] = true
	var dispatched atomic.Int32
	f := newDefaultFixture(t, model, engine.WithTool(llm.Tool{Name: "write", Description: "writes"}, func(context.Context, cortex.Invocation) (string, error) {
		dispatched.Add(1)
		return "written", nil
	}))
	require.NoError(t, f.eng.Start(context.Background()))
	done := f.start()
	model.waitEntered(t)
	require.Nil(t, f.onlyRun(t).Lease, "a run on a default engine holds a lease")

	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stopped <- f.eng.Stop(ctx)
	}()
	probe, cancelProbe := context.WithCancel(leaseScope())
	cancelProbe()
	require.Eventually(t, func() bool {
		_, err := f.eng.RunAgent(probe, "assistant", "late", nil)
		return errors.Is(err, cortex.ErrEngineStopping)
	}, 2*time.Second, 10*time.Millisecond)
	model.letGo()

	out := waitOutcome(t, done)
	require.NoError(t, out.err)
	require.Equal(t, run.StateCompleted, out.run.State)
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return after the run finished")
	}
	require.EqualValues(t, 1, dispatched.Load(), "the run did not finish its tool call while Stop drained")
	r := f.onlyRun(t)
	require.Equal(t, run.StateCompleted, r.State)
	require.Nil(t, r.Lease)
	completed, failed, _, notices := f.hooks.counts()
	require.Equal(t, 1, completed)
	require.Zero(t, failed)
	require.Empty(t, notices, "a default engine announced a reclaimable run")
	reclaimable, err := f.eng.ListRuns(leaseScope(), &run.ListFilter{State: run.StateReclaimable})
	require.NoError(t, err)
	require.Empty(t, reclaimable)
	require.Empty(t, f.store.snapshots(), "a default engine saved progress")
	require.Zero(t, f.store.holds.Load(), "a default engine renewed a lease")
}

// runContinuer is the plugin a host that turns leasing on registers: every
// ready or reclaimable run becomes a job, and the job calls ContinueRun on
// whichever replica takes it, under the run's scope from the hook's ctx.
type runContinuer struct{ jobs chan continueJob }

type continueJob struct {
	scope cortex.Scope
	runID id.AgentRunID
}

func (*runContinuer) Name() string { return "run-continuer" }

func (c *runContinuer) OnRunReady(ctx context.Context, _ id.AgentID, runID id.AgentRunID, _ run.ReadyNotice) error {
	c.enqueue(ctx, runID)
	return nil
}

func (c *runContinuer) OnRunReclaimable(ctx context.Context, _ id.AgentID, runID id.AgentRunID, _ run.ReclaimNotice) error {
	c.enqueue(ctx, runID)
	return nil
}

// enqueue never blocks: hooks run on the engine's own goroutines, Stop's
// hand-off among them. A dropped notice comes back with the sweeper.
func (c *runContinuer) enqueue(ctx context.Context, runID id.AgentRunID) {
	select {
	case c.jobs <- continueJob{scope: cortex.ScopeFromContext(ctx), runID: runID}:
	default:
	}
}

// With WithRunLease set, Stop hands the run off and another replica's job
// finishes it with ContinueRun, as the docs show a host wiring it.
func TestLeasedHandOffIsFinishedByTheHostsContinuer(t *testing.T) {
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-1", Name: "write", Arguments: `{}`}))
	model.hold[0] = true
	var dispatched atomic.Int32
	write := engine.WithTool(llm.Tool{Name: "write", Description: "writes"}, func(context.Context, cortex.Invocation) (string, error) {
		dispatched.Add(1)
		return "written", nil
	})
	continuer := &runContinuer{jobs: make(chan continueJob, 4)}
	f := newLeaseFixture(t, model, write, engine.WithExtension(continuer))
	require.NoError(t, f.eng.Start(context.Background()))
	done := f.start()
	model.waitEntered(t)

	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stopped <- f.eng.Stop(ctx)
	}()
	probe, cancelProbe := context.WithCancel(leaseScope())
	cancelProbe()
	require.Eventually(t, func() bool {
		_, err := f.eng.RunAgent(probe, "assistant", "late", nil)
		return errors.Is(err, cortex.ErrEngineStopping)
	}, 2*time.Second, 10*time.Millisecond)
	model.letGo()
	require.ErrorIs(t, waitOutcome(t, done).err, cortex.ErrRunInterrupted)
	require.NoError(t, <-stopped)
	require.Equal(t, run.StateReclaimable, f.onlyRun(t).State)

	var job continueJob
	select {
	case job = <-continuer.jobs:
	case <-time.After(5 * time.Second):
		t.Fatal("the hand-off queued no job")
	}
	replica, err := engine.New(engine.WithStore(f.store), engine.WithLLM(model), engine.WithWorkerID("replica"), engine.WithRunLease(testLease), write)
	require.NoError(t, err)
	finished, err := replica.ContinueRun(cortex.WithScope(context.Background(), job.scope), job.runID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, finished.State)
	require.EqualValues(t, 1, dispatched.Load(), "the tool ran other than once across the hand-off")
	_, err = f.store.GetProgress(leaseScope(), job.runID)
	require.ErrorIs(t, err, cortex.ErrProgressNotFound)
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

// A call that is not retry-safe is never dispatched without a durable
// Started record. One failed save is retried; when the retry fails too the
// call does not run, nothing is recorded as done, and the pass ends
// without a terminal write, leaving the lease to lapse for a reclaim.
func TestStartedSaveFailureStopsBeforeDispatch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures int32
		ran      int32
	}{
		{"retry succeeds", 1, 1},
		{"retry fails", 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-1", Name: "write", Arguments: `{}`}))
			var ran atomic.Int32
			f := newLeaseFixture(t, model, engine.WithRunLease(300*time.Millisecond), engine.WithTool(llm.Tool{Name: "write", Description: "writes"}, func(context.Context, cortex.Invocation) (string, error) {
				ran.Add(1)
				return "written", nil
			}))
			f.store.failStarted.Store(tc.failures)
			r, err := f.eng.RunAgent(leaseScope(), "assistant", "work", nil)
			require.Equal(t, tc.ran, ran.Load())
			if tc.ran == 1 {
				require.NoError(t, err)
				require.Equal(t, run.StateCompleted, r.State)
				return
			}
			require.ErrorIs(t, err, cortex.ErrRunInterrupted)
			stored := f.onlyRun(t)
			require.Equal(t, run.StateRunning, stored.State, "the pass wrote a state instead of leaving the lease to lapse")
			_, failed, terminated, notices := f.hooks.counts()
			require.Zero(t, failed)
			require.Zero(t, terminated)
			require.Empty(t, notices)
			p, err := f.store.GetProgress(leaseScope(), stored.ID)
			require.NoError(t, err)
			require.Empty(t, p.Started)
			require.Empty(t, p.Done)
			require.Equal(t, 1, p.Cont.StepIndex)
			holds := f.store.holds.Load()
			time.Sleep(400 * time.Millisecond)
			require.Equal(t, holds, f.store.holds.Load(), "the heartbeat kept renewing a lease the pass gave up")
		})
	}
}

// A run started from inside a leased run's tool while Stop drains runs to
// the end: its lifetime follows the tool call that started it. The parent
// records the child's answer and hands off at its next boundary.
func TestNestedRunFinishesWhileStopDrains(t *testing.T) {
	model := newLeaseLLM(
		toolCalls(llm.ToolCall{ID: "call-spawn", Name: "spawn", Arguments: `{}`}),
		&llm.Response{Content: "child done", FinishReason: "stop"},
	)
	entered := make(chan struct{})
	gate := make(chan struct{})
	var eng *engine.Engine
	var child *run.Run
	var childErr error
	f := newLeaseFixture(t, model, engine.WithTool(llm.Tool{Name: "spawn", Description: "runs a nested agent"}, func(ctx context.Context, _ cortex.Invocation) (string, error) {
		close(entered)
		<-gate
		child, childErr = eng.RunAgent(ctx, "assistant", "child", nil)
		if childErr != nil {
			return "", childErr
		}
		return child.Output, nil
	}))
	eng = f.eng
	require.NoError(t, eng.Start(context.Background()))
	done := f.start()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the spawn tool never ran")
	}

	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stopped <- eng.Stop(ctx)
	}()
	probe, cancelProbe := context.WithCancel(leaseScope())
	cancelProbe()
	require.Eventually(t, func() bool {
		_, err := eng.RunAgent(probe, "assistant", "late", nil)
		return errors.Is(err, cortex.ErrEngineStopping)
	}, 2*time.Second, 10*time.Millisecond)
	close(gate)

	out := waitOutcome(t, done)
	require.ErrorIs(t, out.err, cortex.ErrRunInterrupted)
	require.NoError(t, childErr, "the nested run was handed off instead of finishing")
	require.Equal(t, run.StateCompleted, child.State)
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
	parent, err := eng.GetRun(leaseScope(), out.run.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateReclaimable, parent.State)
	p, err := f.store.GetProgress(leaseScope(), parent.ID)
	require.NoError(t, err)
	answered := false
	for _, m := range p.Cont.Messages {
		if m.Role == "tool" && m.ToolCallID == "call-spawn" && m.Content == "child done" {
			answered = true
		}
	}
	require.True(t, answered, "the parent's progress lost the child's answer: %+v", p.Cont.Messages)
}

// A tool whose error says its own nested run was handed off interrupts the
// parent: the call is not recorded as done or failed, and the parent hands
// off with no terminal write.
func TestToolReportingAHandOffInterruptsTheRun(t *testing.T) {
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-1", Name: "delegate", Arguments: `{}`}))
	f := newLeaseFixture(t, model, engine.WithTool(llm.Tool{Name: "delegate", Description: "runs a nested agent"}, func(context.Context, cortex.Invocation) (string, error) {
		return "", fmt.Errorf("nested run: %w", cortex.ErrRunInterrupted)
	}))
	r, err := f.eng.RunAgent(leaseScope(), "assistant", "work", nil)
	require.ErrorIs(t, err, cortex.ErrRunInterrupted)
	require.Equal(t, 1, model.callCount())
	stored := f.onlyRun(t)
	require.Equal(t, run.StateReclaimable, stored.State)
	require.Equal(t, r.ID, stored.ID)
	_, failed, terminated, notices := f.hooks.counts()
	require.Zero(t, failed)
	require.Zero(t, terminated)
	require.Len(t, notices, 1)
	p, err := f.store.GetProgress(leaseScope(), stored.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"call-1"}, startedIDs(p.Started))
	require.Empty(t, p.Done, "the interrupted call was recorded as done")
}

// A resume whose lease cannot be taken after the claim retries briefly;
// when every attempt fails the pass ends with no terminal write, and
// nothing of the resume runs.
// A resume's claim takes the lease, so a store that refuses every renewal
// cannot leave the run running with nobody's lease on it: the lease the
// claim took lapses, the sweep reclaims the run, and another replica
// continues it with the answer the resume carried. The worker whose
// renewals failed finds out at its next write.
func TestResumeWhoseRenewalsFailRecoversThroughTheSweep(t *testing.T) {
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-1", Name: "ask_human", Arguments: `{}`}))
	model.hold[1] = true
	f := newLeaseFixture(t, model, engine.WithRunLease(reclaimTTL), engine.WithExternalTool(llm.Tool{Name: "ask_human", Description: "answered by the host"}))
	paused, err := f.eng.RunAgent(leaseScope(), "assistant", "work", nil)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, paused.State)

	f.store.failHolds.Store(1 << 20)
	holds := f.store.holds.Load()
	done := make(chan runOutcome, 1)
	f.runs.Add(1)
	go func() {
		defer f.runs.Done()
		r, resumeErr := f.eng.Resume(leaseScope(), paused.ID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "call-1", Content: "yes"}}})
		done <- runOutcome{run: r, err: resumeErr}
	}()
	model.waitEntered(t)
	claimed := f.onlyRun(t)
	require.NotNil(t, claimed.Lease, "the claim took no lease")
	require.Equal(t, f.eng.WorkerID(), claimed.Lease.Owner)
	require.Equal(t, holds, f.store.holds.Load(), "the claim asked for the lease separately")
	require.Eventually(t, func() bool {
		r, readErr := f.store.GetRun(leaseScope(), paused.ID)
		return readErr == nil && r.Lease != nil && time.Now().After(r.Lease.ExpiresAt)
	}, 5*time.Second, 5*time.Millisecond, "the lease nobody could renew never lapsed")

	bModel := &reclaimLLM{}
	b := reclaimer(t, f, bModel, newReclaimHooks(), engine.WithExternalTool(llm.Tool{Name: "ask_human", Description: "answered by the host"}))
	engine.SweepOnce(b, context.Background())
	continued, err := b.ContinueRun(leaseScope(), paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, continued.State)
	require.Equal(t, "yes", lastToolMessage(t, bModel.firstRequest(t)).Content, "the answer the resume carried was lost")

	// The worker whose renewals failed wakes up. It cannot tell its lease is
	// gone (its store refuses every renewal), but the run has ended under
	// the replica that reclaimed it, and it adopts that ending instead of
	// writing or announcing one of its own.
	model.letGo()
	if out := waitOutcome(t, done); out.err == nil {
		require.Equal(t, continued.Output, out.run.Output)
	} else {
		require.ErrorIs(t, out.err, cortex.ErrRunLeaseLost)
	}
	completed, _, _, _ := f.hooks.counts()
	require.Zero(t, completed, "the stale worker announced a completion")
	final := f.onlyRun(t)
	require.Equal(t, run.StateCompleted, final.State)
	require.Equal(t, continued.Output, final.Output)
}

func lastToolMessage(t *testing.T, req *llm.Request) llm.Message {
	t.Helper()
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "tool" {
			return req.Messages[i]
		}
	}
	t.Fatal("the request carries no tool message")
	return llm.Message{}
}
