package orchestration

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// executionRunStore copies records, just as a database would. Retaining the
// service's pointer would hide a missing write behind an in-memory mutation.
type executionRunStore struct {
	fakeRunStore
	mu            sync.Mutex
	record        Run
	writes        int
	failWrite     int
	failure       error
	observedScope cortex.Scope
	writeDeadline time.Time
	firstWrite    chan struct{}
}

func (s *executionRunStore) CreateOrchestrationRun(ctx context.Context, r *Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.record = copyExecutionRun(r)
	return nil
}

func (s *executionRunStore) UpdateOrchestrationRun(ctx context.Context, r *Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if s.writes == 1 && s.firstWrite != nil {
		close(s.firstWrite)
	}
	if s.writes == s.failWrite {
		return s.failure
	}
	s.observedScope = cortex.ScopeFromContext(ctx)
	s.writeDeadline, _ = ctx.Deadline()
	s.record = copyExecutionRun(r)
	return nil
}

func (s *executionRunStore) snapshot() Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyExecutionRun(&s.record)
}

func copyExecutionRun(r *Run) Run {
	out := *r
	out.AgentRunIDs = slices.Clone(r.AgentRunIDs)
	if r.CompletedAt != nil {
		completed := *r.CompletedAt
		out.CompletedAt = &completed
	}
	return out
}

func executionService(runner AgentRunner, runs RunStore, hooks HookEmitter, strategy string) *Service {
	cfg := &Config{ID: id.NewOrchestrationConfigID(), Name: "team", Strategy: strategy,
		Participants: []Participant{{AgentName: "a"}, {AgentName: "b"}}}
	return NewService(runner, &fakeConfigStore{byName: map[string]*Config{"team": cfg}}, runs, hooks)
}

func TestServicePersistsChildBeforeNextDispatch(t *testing.T) {
	runs := &executionRunStore{}
	first, second := id.NewAgentRunID(), id.NewAgentRunID()
	runner := hierarchyRunnerFunc(func(_ context.Context, name, _ string, _ *RunOpts) (*AgentResult, error) {
		key := first
		if name == "b" {
			key = second
			record := runs.snapshot()
			if record.Status != StatusRunning || !slices.Equal(record.AgentRunIDs, []id.AgentRunID{first}) {
				t.Errorf("first child not durable before second dispatch: %+v", record)
			}
		}
		return &AgentResult{AgentName: name, RunID: key, Output: "answer"}, nil
	})
	svc := executionService(runner, runs, nil, StrategySequential)
	rec, err := svc.Run(context.Background(), "team", "work")
	if err != nil || rec == nil || !slices.Equal(rec.AgentRunIDs, []id.AgentRunID{first, second}) {
		t.Fatalf("run: %+v %v", rec, err)
	}
}

func TestServiceCancellationRetainsFailedChild(t *testing.T) {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "cancelled-team"}}}
	ctx, cancel := context.WithCancel(cortex.WithScope(context.Background(), scope))
	defer cancel()
	child := id.NewAgentRunID()
	runs := &executionRunStore{}
	hooks := &recordingHooks{}
	runner := hierarchyRunnerFunc(func(_ context.Context, name, _ string, _ *RunOpts) (*AgentResult, error) {
		cancel()
		return &AgentResult{AgentName: name, RunID: child, Output: "partial"}, context.Canceled
	})
	svc := executionService(runner, runs, hooks, StrategySequential)
	rec, err := svc.Run(ctx, "team", "work")
	if !errors.Is(err, context.Canceled) || rec == nil {
		t.Fatalf("run: %+v %v", rec, err)
	}
	stored := runs.snapshot()
	if stored.Status != StatusFailed || stored.CompletedAt == nil || stored.Output != "" || !slices.Equal(stored.AgentRunIDs, []id.AgentRunID{child}) {
		t.Fatalf("cancelled team not finalized: %+v", stored)
	}
	if runs.observedScope.Canonical() != scope.Canonical() || runs.writeDeadline.IsZero() || time.Until(runs.writeDeadline) > 5*time.Second {
		t.Fatalf("final write lost scope or timeout: scope=%v deadline=%v", runs.observedScope, runs.writeDeadline)
	}
	if hooks.completed != 1 {
		t.Fatalf("completion hooks: %d", hooks.completed)
	}
}

func TestServiceChildWriteFailureStopsFollowingDispatch(t *testing.T) {
	failure := errors.New("child evidence unavailable")
	runs := &executionRunStore{failWrite: 1, failure: failure}
	child := id.NewAgentRunID()
	calls := 0
	runner := hierarchyRunnerFunc(func(_ context.Context, name, _ string, _ *RunOpts) (*AgentResult, error) {
		calls++
		return &AgentResult{AgentName: name, RunID: child, Output: "answer"}, nil
	})
	svc := executionService(runner, runs, nil, StrategySequential)
	rec, err := svc.Run(context.Background(), "team", "work")
	if !errors.Is(err, failure) || calls != 1 || rec == nil || rec.Status != StatusFailed || rec.Output != "" {
		t.Fatalf("dispatch continued after failed evidence write: calls=%d rec=%+v err=%v", calls, rec, err)
	}
	stored := runs.snapshot()
	if stored.Status != StatusFailed || !slices.Equal(stored.AgentRunIDs, []id.AgentRunID{child}) {
		t.Fatalf("final retry lost failed child: %+v", stored)
	}
}

func TestServiceParallelWriteFailureKeepsInflightEvidence(t *testing.T) {
	failure := errors.New("first write acknowledgment lost")
	firstWrite := make(chan struct{})
	runs := &executionRunStore{failWrite: 1, failure: failure, firstWrite: firstWrite}
	first, second := id.NewAgentRunID(), id.NewAgentRunID()
	var admitted sync.WaitGroup
	admitted.Add(2)
	runner := hierarchyRunnerFunc(func(ctx context.Context, name, _ string, _ *RunOpts) (*AgentResult, error) {
		admitted.Done()
		admitted.Wait()
		key := first
		if name == "b" {
			select {
			case <-firstWrite:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			key = second
		}
		return &AgentResult{AgentName: name, RunID: key, Output: "answer"}, nil
	})
	svc := executionService(runner, runs, nil, StrategyParallel)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rec, err := svc.Run(ctx, "team", "work")
	if !errors.Is(err, failure) || rec == nil || rec.Status != StatusFailed || rec.Output != "" {
		t.Fatalf("parallel failure: %+v %v", rec, err)
	}
	stored := runs.snapshot()
	if stored.Status != StatusFailed || !slices.Equal(stored.AgentRunIDs, []id.AgentRunID{first, second}) {
		t.Fatalf("in-flight evidence lost: %+v", stored)
	}
}

func TestServiceFinalWriteFailureKeepsExecutionError(t *testing.T) {
	writeFailure, runFailure := errors.New("final write failed"), errors.New("agent failed")
	runs := &executionRunStore{failWrite: 2, failure: writeFailure}
	child := id.NewAgentRunID()
	runner := hierarchyRunnerFunc(func(_ context.Context, name, _ string, _ *RunOpts) (*AgentResult, error) {
		return &AgentResult{AgentName: name, RunID: child}, runFailure
	})
	hooks := &recordingHooks{}
	svc := executionService(runner, runs, hooks, StrategySequential)
	rec, err := svc.Run(context.Background(), "team", "work")
	if !errors.Is(err, writeFailure) || !errors.Is(err, runFailure) || rec == nil || rec.Status != StatusFailed || hooks.completed != 0 {
		t.Fatalf("final write failure concealed: %+v %v hooks=%d", rec, err, hooks.completed)
	}
	stored := runs.snapshot()
	if stored.Status != StatusRunning || !slices.Equal(stored.AgentRunIDs, []id.AgentRunID{child}) {
		t.Fatalf("successful intermediate write missing: %+v", stored)
	}
}

type startHook struct {
	noopHooks
	start func()
}

func (h startHook) OrchestrationStarted(context.Context, id.OrchestrationID, string) { h.start() }

func TestServiceRunConfigUsesCapturedExecutionFields(t *testing.T) {
	for _, strategy := range []string{StrategySequential, StrategyParallel, StrategyRouter, StrategyHierarchical, StrategyDebate} {
		t.Run(strategy, func(t *testing.T) {
			var mu sync.Mutex
			var called []string
			calls := 0
			runner := hierarchyRunnerFunc(func(_ context.Context, name, input string, opts *RunOpts) (*AgentResult, error) {
				mu.Lock()
				defer mu.Unlock()
				called = append(called, name)
				calls++
				if opts == nil || opts.Model != "frozen-model" || strings.Contains(input, "injected-role") {
					t.Errorf("configuration changed at dispatch: opts=%+v input=%s", opts, input)
				}
				output := "answer"
				if strategy == StrategyHierarchical && calls == 1 {
					output = `[{"agent":"b","task":"work"}]`
				}
				return &AgentResult{AgentName: name, RunID: id.NewAgentRunID(), Output: output}, nil
			})
			cfg := Config{ID: id.NewOrchestrationConfigID(), Strategy: strategy,
				Participants: []Participant{{AgentName: "a"}, {AgentName: "b"}},
				Settings:     Settings{Model: "frozen-model", RouterRules: map[string]string{"work": "b"}}}
			hooks := startHook{start: func() {
				cfg.Participants[1].AgentName = "outsider"
				cfg.Participants[0].Role = "injected-role"
				cfg.Settings.RouterRules["work"] = "a"
				cfg.Settings.Model = "live-model"
			}}
			svc := NewService(runner, nil, &executionRunStore{}, hooks)
			captured, ok := any(svc).(interface {
				RunConfig(context.Context, Config, string) (*Run, error)
			})
			if !ok {
				t.Fatal("captured configuration execution unavailable")
			}
			rec, err := captured.RunConfig(context.Background(), cfg, "work")
			if err != nil || rec == nil || rec.Status != StatusCompleted || len(rec.AgentRunIDs) != calls {
				t.Fatalf("strategy failed: %+v %v calls=%d", rec, err, calls)
			}
			if slices.Contains(called, "outsider") || (strategy == StrategyRouter && !slices.Equal(called, []string{"b"})) {
				t.Fatalf("live participants or rules leaked: %v", called)
			}
		})
	}
}
