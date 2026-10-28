package engine_test

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
)

// admissionLLM holds every model call until the test lets it go, and
// records how many calls were inside it at once. That peak is the number
// of runs that were really executing in parallel.
type admissionLLM struct {
	mu      sync.Mutex
	active  int
	peak    int
	entered chan struct{}
	// release hands out one token per call; closing it lets every call
	// through, now and later.
	release chan struct{}
	respond func(*llm.Request) *llm.Response
}

func newAdmissionLLM() *admissionLLM {
	return &admissionLLM{
		entered: make(chan struct{}, 64),
		release: make(chan struct{}),
		respond: func(*llm.Request) *llm.Response { return &llm.Response{Content: "done", FinishReason: "stop"} },
	}
}

func (m *admissionLLM) enter(ctx context.Context) error {
	m.mu.Lock()
	m.active++
	if m.active > m.peak {
		m.peak = m.active
	}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.active--
		m.mu.Unlock()
	}()
	m.entered <- struct{}{}
	select {
	case <-m.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *admissionLLM) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	if err := m.enter(ctx); err != nil {
		return nil, err
	}
	return m.respond(req), nil
}

func (m *admissionLLM) CompleteStream(ctx context.Context, req *llm.Request) (llm.Stream, error) {
	if err := m.enter(ctx); err != nil {
		return nil, err
	}
	resp := m.respond(req)
	return &admissionStream{chunk: &llm.Chunk{Content: resp.Content, FinishReason: resp.FinishReason}}, nil
}

func (m *admissionLLM) maxParallel() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peak
}

// letAllThrough is safe to call twice, so a test can release early and
// still leave the cleanup in place.
func (m *admissionLLM) letAllThrough() {
	defer func() { _ = recover() }()
	close(m.release)
}

type admissionStream struct{ chunk *llm.Chunk }

func (s *admissionStream) Next(context.Context) (*llm.Chunk, error) {
	if s.chunk == nil {
		return nil, io.EOF
	}
	c := s.chunk
	s.chunk = nil
	return c, nil
}

func (s *admissionStream) Close() error      { return nil }
func (s *admissionStream) Usage() *llm.Usage { return nil }

// shutdownCounter counts shutdown notifications, so a test can tell a
// Stop that finished from one that gave up at its deadline.
type shutdownCounter struct{ n atomic.Int32 }

func (*shutdownCounter) Name() string { return "shutdown-counter" }

func (c *shutdownCounter) OnShutdown(context.Context) error {
	c.n.Add(1)
	return nil
}

func admissionScope() context.Context {
	return cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_admission"}}})
}

// newAdmissionEngine builds an engine over a migrated SQLite store with
// the agents "assistant" and "worker". The model is released when the
// test ends, and every run the test started is waited for before the
// store closes.
func newAdmissionEngine(t *testing.T, model *admissionLLM, runs *sync.WaitGroup, opts ...engine.Option) *engine.Engine {
	t.Helper()
	// Several runs finish at once here, so writers wait for the lock
	// instead of failing with SQLITE_BUSY.
	s := openSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "admission.db")+"?_pragma=busy_timeout(10000)")
	t.Cleanup(func() {
		model.letAllThrough()
		runs.Wait()
	})
	eng, err := engine.New(append([]engine.Option{engine.WithStore(s), engine.WithLLM(model)}, opts...)...)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	for _, name := range []string{"assistant", "worker"} {
		if err := eng.CreateAgent(admissionScope(), &agent.Config{ID: id.NewAgentID(), Name: name, MaxSteps: 3}); err != nil {
			t.Fatalf("create agent %s: %v", name, err)
		}
	}
	return eng
}

type runOutcome struct {
	run *run.Run
	err error
}

func startRun(ctx context.Context, eng *engine.Engine, runs *sync.WaitGroup, input string) <-chan runOutcome {
	done := make(chan runOutcome, 1)
	runs.Add(1)
	go func() {
		defer runs.Done()
		r, err := eng.RunAgent(ctx, "assistant", input, nil)
		done <- runOutcome{run: r, err: err}
	}()
	return done
}

func waitEntered(t *testing.T, model *admissionLLM, what string) {
	t.Helper()
	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s to reach the model", what)
	}
}

func waitOutcome(t *testing.T, done <-chan runOutcome) runOutcome {
	t.Helper()
	select {
	case out := <-done:
		return out
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a run to return")
		return runOutcome{}
	}
}

func storedState(t *testing.T, eng *engine.Engine, runID id.AgentRunID) run.State {
	t.Helper()
	r, err := eng.GetRun(admissionScope(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	return r.State
}

func onlyRun(t *testing.T, eng *engine.Engine) *run.Run {
	t.Helper()
	rows, err := eng.ListRuns(admissionScope(), &run.ListFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListRuns = %d rows, want 1", len(rows))
	}
	return rows[0]
}

func TestWithRunConcurrencyRejectsNegative(t *testing.T) {
	if _, err := engine.New(engine.WithRunConcurrency(-1)); err == nil {
		t.Fatal("WithRunConcurrency(-1) was accepted")
	}
	eng, err := engine.New()
	if err != nil {
		t.Fatal(err)
	}
	if got := eng.RunConcurrency(); got != 0 {
		t.Fatalf("default RunConcurrency = %d, want 0 (unlimited)", got)
	}
}

func TestRunConcurrencyLimitsParallelRuns(t *testing.T) {
	model := newAdmissionLLM()
	var runs sync.WaitGroup
	eng := newAdmissionEngine(t, model, &runs, engine.WithRunConcurrency(2))
	if got := eng.RunConcurrency(); got != 2 {
		t.Fatalf("RunConcurrency = %d, want 2", got)
	}

	ctx := admissionScope()
	outs := []<-chan runOutcome{startRun(ctx, eng, &runs, "one"), startRun(ctx, eng, &runs, "two"), startRun(ctx, eng, &runs, "three")}
	waitEntered(t, model, "the first run")
	waitEntered(t, model, "the second run")
	select {
	case <-model.entered:
		t.Fatal("a third run reached the model while two held both slots")
	case <-time.After(100 * time.Millisecond):
	}

	// Let exactly one call go. Its run finishes, gives its slot back, and
	// only then may the third run start.
	model.release <- struct{}{}
	waitEntered(t, model, "the third run")
	model.letAllThrough()
	for _, done := range outs {
		if out := waitOutcome(t, done); out.err != nil {
			t.Fatalf("RunAgent: %v", out.err)
		}
	}
	if got := model.maxParallel(); got != 2 {
		t.Fatalf("model saw %d calls at once, want 2", got)
	}
}

func TestRunConcurrencyWaitHonoursContext(t *testing.T) {
	model := newAdmissionLLM()
	var runs sync.WaitGroup
	eng := newAdmissionEngine(t, model, &runs, engine.WithRunConcurrency(1))

	held := startRun(admissionScope(), eng, &runs, "holds the slot")
	waitEntered(t, model, "the slot holder")

	ctx, cancel := context.WithTimeout(admissionScope(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := eng.RunAgent(ctx, "assistant", "waits", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunAgent with a 50ms ctx = %v, want context.DeadlineExceeded", err)
	}
	if waited := time.Since(started); waited > time.Second {
		t.Fatalf("a refused run waited %s past its deadline", waited)
	}
	count, err := eng.CountRuns(admissionScope(), &run.ListFilter{})
	if err != nil {
		t.Fatalf("CountRuns: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountRuns = %d, want 1: a run that never got a slot must not leave a row", count)
	}

	model.letAllThrough()
	if out := waitOutcome(t, held); out.err != nil {
		t.Fatalf("slot holder: %v", out.err)
	}
}

func TestRunConcurrencyNestedRunDoesNotDeadlock(t *testing.T) {
	model := newAdmissionLLM()
	model.letAllThrough()
	model.respond = func(req *llm.Request) *llm.Response {
		last := req.Messages[len(req.Messages)-1]
		if last.Role == "user" && last.Content == "parent" {
			return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "call-spawn", Name: "spawn", Arguments: "{}"}}, FinishReason: "tool_calls"}
		}
		return &llm.Response{Content: "done", FinishReason: "stop"}
	}

	var eng *engine.Engine
	var child *run.Run
	spawn := engine.WithTool(llm.Tool{Name: "spawn", Description: "runs the worker agent"}, func(ctx context.Context, _ cortex.Invocation) (string, error) {
		r, err := eng.RunAgent(ctx, "worker", "child", nil)
		if err != nil {
			return "", err
		}
		child = r
		return r.Output, nil
	})
	var runs sync.WaitGroup
	eng = newAdmissionEngine(t, model, &runs, engine.WithRunConcurrency(1), spawn)

	// A deadlock shows up as the wait running out, not as a hung test.
	ctx, cancel := context.WithTimeout(admissionScope(), 5*time.Second)
	defer cancel()
	parent, err := eng.RunAgent(ctx, "assistant", "parent", nil)
	if err != nil {
		t.Fatalf("parent RunAgent: %v", err)
	}
	if parent.State != run.StateCompleted {
		t.Fatalf("parent state = %s, want completed", parent.State)
	}
	if child == nil || child.State != run.StateCompleted {
		t.Fatalf("nested run = %+v, want a completed run", child)
	}
	count, err := eng.CountRuns(admissionScope(), &run.ListFilter{})
	if err != nil {
		t.Fatalf("CountRuns: %v", err)
	}
	if count != 2 {
		t.Fatalf("CountRuns = %d, want 2", count)
	}
}

// A run that is executing when Stop starts finishes before Stop returns,
// and its row says completed: its model call ends the run, so it reaches
// no boundary where a leased run would hand off.
func TestStopRefusesNewRunsAndWaitsForInflight(t *testing.T) {
	model := newAdmissionLLM()
	var runs sync.WaitGroup
	hook := &shutdownCounter{}
	eng := newAdmissionEngine(t, model, &runs, engine.WithExtension(hook))
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inflight := startRun(admissionScope(), eng, &runs, "in flight")
	waitEntered(t, model, "the in-flight run")

	stopped := make(chan error, 1)
	go func() { stopped <- eng.Stop(context.Background()) }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop returned %v while a run was still executing", err)
	case <-time.After(100 * time.Millisecond):
	}

	if _, err := eng.RunAgent(admissionScope(), "assistant", "too late", nil); !errors.Is(err, cortex.ErrEngineStopping) {
		t.Fatalf("RunAgent during Stop = %v, want ErrEngineStopping", err)
	}
	if hook.n.Load() != 0 {
		t.Fatal("shutdown was announced while a run was still executing")
	}

	model.letAllThrough()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after the in-flight run finished")
	}
	out := waitOutcome(t, inflight)
	if out.err != nil {
		t.Fatalf("in-flight run: %v", out.err)
	}
	if got := storedState(t, eng, out.run.ID); got != run.StateCompleted {
		t.Fatalf("stored state of the in-flight run = %s, want completed", got)
	}
	if hook.n.Load() != 1 {
		t.Fatalf("shutdown announced %d times, want 1", hook.n.Load())
	}
	if count, err := eng.CountRuns(admissionScope(), &run.ListFilter{}); err != nil || count != 1 {
		t.Fatalf("CountRuns = %d, %v; the refused run must leave no row", count, err)
	}
}

// When Stop gives up at its deadline, a run with no lease is still
// executing in this process: there is no lease to hand it off under. Its
// row stays running, nothing is lost, and the run writes its own
// completion when it finishes. A second Stop then completes the shutdown.
// A leased run is handed off instead; TestStopDeadlineCancelsBlockedRun
// covers that.
func TestStopReturnsWhenDeadlinePasses(t *testing.T) {
	model := newAdmissionLLM()
	var runs sync.WaitGroup
	hook := &shutdownCounter{}
	eng := newAdmissionEngine(t, model, &runs, engine.WithExtension(hook), engine.WithRunLease(0))
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	inflight := startRun(admissionScope(), eng, &runs, "never ends on its own")
	waitEntered(t, model, "the in-flight run")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := eng.Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop at its deadline = %v, want context.DeadlineExceeded", err)
	}
	if took := time.Since(started); took > time.Second {
		t.Fatalf("Stop took %s, want it to give up at its 100ms deadline", took)
	}
	if hook.n.Load() != 0 {
		t.Fatal("a Stop that gave up still announced shutdown")
	}
	if got := onlyRun(t, eng).State; got != run.StateRunning {
		t.Fatalf("stored state after the deadline = %s, want running (the run is still executing)", got)
	}
	if _, err := eng.RunAgent(admissionScope(), "assistant", "too late", nil); !errors.Is(err, cortex.ErrEngineStopping) {
		t.Fatalf("RunAgent after a timed-out Stop = %v, want ErrEngineStopping", err)
	}

	model.letAllThrough()
	out := waitOutcome(t, inflight)
	if out.err != nil {
		t.Fatalf("in-flight run: %v", out.err)
	}
	if got := storedState(t, eng, out.run.ID); got != run.StateCompleted {
		t.Fatalf("stored state once the run finished = %s, want completed", got)
	}
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if hook.n.Load() != 1 {
		t.Fatalf("shutdown announced %d times after the second Stop, want 1", hook.n.Load())
	}
}

func TestStartAfterStopAdmitsAgain(t *testing.T) {
	model := newAdmissionLLM()
	model.letAllThrough()
	var runs sync.WaitGroup
	eng := newAdmissionEngine(t, model, &runs, engine.WithRunConcurrency(1))
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := eng.RunAgent(admissionScope(), "assistant", "stopped", nil); !errors.Is(err, cortex.ErrEngineStopping) {
		t.Fatalf("RunAgent on a stopped engine = %v, want ErrEngineStopping", err)
	}
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	r, err := eng.RunAgent(admissionScope(), "assistant", "started again", nil)
	if err != nil {
		t.Fatalf("RunAgent after Start: %v", err)
	}
	if r.State != run.StateCompleted {
		t.Fatalf("state = %s, want completed", r.State)
	}
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("final Stop: %v", err)
	}
}

// A stream keeps its slot until its goroutine closes the channel, not
// until StreamAgent returns, which happens as soon as the run starts.
func TestRunConcurrencyStreamHoldsSlotUntilClosed(t *testing.T) {
	model := newAdmissionLLM()
	var runs sync.WaitGroup
	eng := newAdmissionEngine(t, model, &runs, engine.WithRunConcurrency(1))

	events := make(chan engine.StreamEvent, 64)
	if err := eng.StreamAgent(admissionScope(), "assistant", "streamed", nil, events); err != nil {
		t.Fatalf("StreamAgent: %v", err)
	}
	waitEntered(t, model, "the stream")

	ctx, cancel := context.WithTimeout(admissionScope(), 50*time.Millisecond)
	defer cancel()
	if _, err := eng.RunAgent(ctx, "assistant", "waits for the stream", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunAgent while a stream held the only slot = %v, want context.DeadlineExceeded", err)
	}

	model.letAllThrough()
	for ev := range events {
		_ = ev
	}
	r, err := eng.RunAgent(admissionScope(), "assistant", "after the stream", nil)
	if err != nil {
		t.Fatalf("RunAgent after the stream closed: %v", err)
	}
	if r.State != run.StateCompleted {
		t.Fatalf("state = %s, want completed", r.State)
	}
}

// Resume is admitted like a new run: a paused run cannot be continued
// while the engine stops, and stays paused for later.
func TestResumeRefusedWhileStopping(t *testing.T) {
	model := newAdmissionLLM()
	model.letAllThrough()
	model.respond = func(req *llm.Request) *llm.Response {
		if req.Messages[len(req.Messages)-1].Role == "user" {
			return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "call-ask", Name: "ask_human", Arguments: "{}"}}, FinishReason: "tool_calls"}
		}
		return &llm.Response{Content: "done", FinishReason: "stop"}
	}
	var runs sync.WaitGroup
	eng := newAdmissionEngine(t, model, &runs, engine.WithExternalTool(llm.Tool{Name: "ask_human", Description: "answered by the host"}))

	paused, err := eng.RunAgent(admissionScope(), "assistant", "ask", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if paused.State != run.StatePaused {
		t.Fatalf("state = %s, want paused", paused.State)
	}
	if err = eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	in := engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "call-ask", Content: "yes"}}}
	if _, err = eng.Resume(admissionScope(), paused.ID, in); !errors.Is(err, cortex.ErrEngineStopping) {
		t.Fatalf("Resume while stopped = %v, want ErrEngineStopping", err)
	}
	if got := storedState(t, eng, paused.ID); got != run.StatePaused {
		t.Fatalf("stored state after a refused resume = %s, want paused", got)
	}
	if err = eng.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	resumed, err := eng.Resume(admissionScope(), paused.ID, in)
	if err != nil {
		t.Fatalf("Resume after Start: %v", err)
	}
	if resumed.State != run.StateCompleted {
		t.Fatalf("resumed state = %s, want completed", resumed.State)
	}
	if err := eng.Stop(context.Background()); err != nil {
		t.Fatalf("final Stop: %v", err)
	}
}
