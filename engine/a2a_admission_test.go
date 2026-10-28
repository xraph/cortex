package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/suspension"
)

// drainingLLM answers as planner, worker or busy, keyed on the system
// prompt. The planner asks the worker once and then answers with what it
// heard; the worker and busy agents can be held until the test lets them go.
type drainingLLM struct {
	workerCalls atomic.Int32
	holdWorker  bool
	workerIn    chan struct{}
	busyIn      chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func newDrainingLLM(holdWorker bool) *drainingLLM {
	return &drainingLLM{holdWorker: holdWorker, workerIn: make(chan struct{}, 4), busyIn: make(chan struct{}, 4), release: make(chan struct{})}
}

func (l *drainingLLM) letGo() { l.releaseOnce.Do(func() { close(l.release) }) }

func (l *drainingLLM) wait(ctx context.Context) error {
	select {
	case <-l.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *drainingLLM) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	switch {
	case strings.Contains(req.System, "busy"):
		l.busyIn <- struct{}{}
		if err := l.wait(ctx); err != nil {
			return nil, err
		}
		return &llm.Response{Content: "busy done"}, nil
	case strings.Contains(req.System, "worker"):
		l.workerCalls.Add(1)
		l.workerIn <- struct{}{}
		if l.holdWorker {
			if err := l.wait(ctx); err != nil {
				return nil, err
			}
		}
		return &llm.Response{Content: "all clear, nothing burning"}, nil
	}
	if last := req.Messages[len(req.Messages)-1]; last.Role == "tool" {
		return &llm.Response{Content: "the worker says: " + last.Content}, nil
	}
	return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "call-1", Name: toolAgentAsk, Arguments: `{"to":"worker","content":"what is the status?"}`}}}, nil
}

func (l *drainingLLM) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, errors.New("drainingLLM: CompleteStream not supported")
}

func newDrainingEngine(ctx context.Context, t *testing.T, st store.Store, model llm.Client, createAgents bool, opts ...Option) *Engine {
	t.Helper()
	e, err := New(append([]Option{WithStore(st), WithLLM(model), WithA2A(a2a.Options{HopCeiling: 6, Workers: 1})}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if createAgents {
		for _, name := range []string{"planner", "worker", "busy"} {
			if createErr := e.CreateAgent(ctx, &agent.Config{ID: id.NewAgentID(), Name: name, SystemPrompt: "you are " + name, Model: "test-model", MaxSteps: 4}); createErr != nil {
				t.Fatalf("CreateAgent %s: %v", name, createErr)
			}
		}
	}
	return e
}

func a2aScope() context.Context {
	return cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}})
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func assertNoStoppingFailure(ctx context.Context, t *testing.T, st store.Store, runID id.AgentRunID) {
	t.Helper()
	r, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if r.State != run.StateCompleted {
		t.Fatalf("planner state = %s (error %q), want completed", r.State, r.Error)
	}
	if strings.Contains(r.Output, "stopping") || !strings.Contains(r.Output, "all clear") {
		t.Fatalf("planner output = %q, want the worker's real answer", r.Output)
	}
}

// The reviewer's probe: the worker's run is executing when Stop starts, and
// its reply lands during the drain. The reply's resume follows a claim the
// bus made under admission, so it runs through stopping instead of leaving
// the planner paused behind a consumed ask. Without a lease the planner
// completes, and Stop returns once it has. With one (the default) the
// resumed planner records the reply and then hands itself off at its next
// model call: it is reclaimable, and its progress carries the answer.
func TestA2AReplyDuringDrainResumesAsker(t *testing.T) {
	t.Run("without a lease", func(t *testing.T) {
		st, planner := replyDuringDrain(t, WithRunLease(0))
		assertNoStoppingFailure(a2aScope(), t, st, planner)
	})
	t.Run("leased", func(t *testing.T) {
		st, planner := replyDuringDrain(t)
		ctx := a2aScope()
		r, err := st.GetRun(ctx, planner)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if r.State != run.StateReclaimable {
			t.Fatalf("planner state = %s (error %q), want reclaimable", r.State, r.Error)
		}
		saved, err := st.(suspension.ProgressStore).GetProgress(ctx, planner)
		if err != nil {
			t.Fatalf("GetProgress: %v", err)
		}
		answered := false
		for _, m := range saved.Cont.Messages {
			if m.Role == "tool" && strings.Contains(m.Content, "all clear") {
				answered = true
			}
		}
		if !answered {
			t.Fatalf("the handed-off planner's progress lost the worker's answer: %+v", saved.Cont.Messages)
		}
	})
}

func replyDuringDrain(t *testing.T, opts ...Option) (store.Store, id.AgentRunID) {
	t.Helper()
	ctx := a2aScope()
	st := newApprovalStore(ctx, t)
	model := newDrainingLLM(true)
	t.Cleanup(model.letGo)
	e := newDrainingEngine(ctx, t, st, model, true, opts...)

	paused, err := e.RunAgent(ctx, "planner", "find out how the worker is doing", nil)
	if err != nil || paused.State != run.StatePaused {
		t.Fatalf("RunAgent = %v, %v; want a paused planner", paused, err)
	}

	drained := make(chan error, 1)
	go func() {
		_, drainErr := e.A2A().Drain(ctx)
		drained <- drainErr
	}()
	waitSignal(t, model.workerIn, "the worker's run")

	stopped := make(chan error, 1)
	go func() { stopped <- e.Stop(context.Background()) }()
	select {
	case stopErr := <-stopped:
		t.Fatalf("Stop returned %v while the worker's run was executing", stopErr)
	case <-time.After(100 * time.Millisecond):
	}

	model.letGo()
	for _, wait := range []struct {
		ch   chan error
		what string
	}{{drained, "Drain"}, {stopped, "Stop"}} {
		select {
		case waitErr := <-wait.ch:
			if waitErr != nil {
				t.Fatalf("%s: %v", wait.what, waitErr)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s did not return", wait.what)
		}
	}
	if got := model.workerCalls.Load(); got != 1 {
		t.Fatalf("worker ran %d times, want 1", got)
	}
	return st, paused.ID
}

// A directive that arrives while one replica drains is not claimed there
// and draws no Failure reply. Another replica on the same database carries
// it: the worker runs once and the planner completes on the real answer.
func TestA2ADirectiveDuringDrainGoesToAnotherReplica(t *testing.T) {
	ctx := a2aScope()
	st := newApprovalStore(ctx, t)
	model := newDrainingLLM(false)
	t.Cleanup(model.letGo)
	draining := newDrainingEngine(ctx, t, st, model, true)
	replica := newDrainingEngine(ctx, t, st, model, false)

	paused, err := draining.RunAgent(ctx, "planner", "find out how the worker is doing", nil)
	if err != nil || paused.State != run.StatePaused {
		t.Fatalf("RunAgent = %v, %v; want a paused planner", paused, err)
	}

	// A run that keeps the first replica draining.
	busy := make(chan error, 1)
	go func() {
		_, runErr := draining.RunAgent(ctx, "busy", "hold on", nil)
		busy <- runErr
	}()
	waitSignal(t, model.busyIn, "the busy run")
	stopped := make(chan error, 1)
	go func() { stopped <- draining.Stop(context.Background()) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		draining.admission.mu.Lock()
		started := draining.admission.stopping
		draining.admission.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first replica never started draining")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, drainErr := draining.A2A().Drain(ctx); !errors.Is(drainErr, cortex.ErrEngineStopping) {
		t.Fatalf("Drain on the draining replica = %v, want ErrEngineStopping", drainErr)
	}
	if got := model.workerCalls.Load(); got != 0 {
		t.Fatalf("the draining replica ran the worker %d times, want 0", got)
	}
	if r, getErr := st.GetRun(ctx, paused.ID); getErr != nil || r.State != run.StatePaused {
		t.Fatalf("planner = %v, %v; want still paused, waiting for the answer", r, getErr)
	}

	if _, drainErr := replica.A2A().Drain(ctx); drainErr != nil {
		t.Fatalf("Drain on the other replica: %v", drainErr)
	}
	assertNoStoppingFailure(ctx, t, st, paused.ID)
	if got := model.workerCalls.Load(); got != 1 {
		t.Fatalf("worker ran %d times, want exactly 1", got)
	}

	model.letGo()
	if runErr := <-busy; runErr != nil {
		t.Fatalf("busy run: %v", runErr)
	}
	select {
	case stopErr := <-stopped:
		if stopErr != nil {
			t.Fatalf("Stop: %v", stopErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return")
	}
}
