package orchestration

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

type reservingRunnerFunc func(context.Context, id.OrchestrationID, string, string, *RunOpts) (*ReservedInvocation, error)

func (f reservingRunnerFunc) ReserveAgent(ctx context.Context, team id.OrchestrationID, name, input string, opts *RunOpts) (*ReservedInvocation, error) {
	return f(ctx, team, name, input, opts)
}

func reservedConfig(strategy string) Config {
	return Config{Strategy: strategy, Participants: []Participant{{AgentName: "a"}, {AgentName: "b"}}}
}

func TestServiceReservedChildIsDurableBeforeExecution(t *testing.T) {
	for _, strategy := range []string{StrategySequential, StrategyParallel, StrategyRouter, StrategyHierarchical, StrategyDebate} {
		t.Run(strategy, func(t *testing.T) {
			runs := &executionRunStore{}
			team := id.NewOrchestrationID()
			var mu sync.Mutex
			calls := 0
			runner := reservingRunnerFunc(func(_ context.Context, teamID id.OrchestrationID, name, _ string, _ *RunOpts) (*ReservedInvocation, error) {
				if teamID != team {
					t.Errorf("budget account changed: got %s want %s", teamID, team)
				}
				child := id.NewAgentRunID()
				return &ReservedInvocation{RunID: child, Execute: func(context.Context) (*AgentResult, error) {
					stored := runs.snapshot()
					if stored.ID != team || stored.Status != StatusRunning || !slices.Contains(stored.AgentRunIDs, child) {
						t.Errorf("child started before durable linkage: %+v", stored)
					}
					mu.Lock()
					defer mu.Unlock()
					calls++
					output := "answer"
					if strategy == StrategyHierarchical && calls == 1 {
						output = `[{"agent":"b","task":"work"}]`
					}
					return &AgentResult{AgentName: name, RunID: child, Output: output}, nil
				}}, nil
			})
			cfg := reservedConfig(strategy)
			cfg.Settings.RouterRules = map[string]string{"work": "b"}
			svc := NewReservedService(runner, runs, nil)
			rec, err := svc.RunConfigWithID(context.Background(), cfg, "work", team)
			if err != nil || rec == nil || rec.ID != team || rec.Status != StatusCompleted || len(rec.AgentRunIDs) != calls || calls == 0 {
				t.Fatalf("reserved execution: %+v %v calls=%d", rec, err, calls)
			}
		})
	}
}

func TestServiceReservedAdmissionFailuresKeepIdentity(t *testing.T) {
	for _, mode := range []string{"write", "reserve", "cancel", "execute"} {
		t.Run(mode, func(t *testing.T) {
			failure := errors.New("admission failed")
			runs := &executionRunStore{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			child := id.NewAgentRunID()
			reservations, executions := 0, 0
			runner := reservingRunnerFunc(func(context.Context, id.OrchestrationID, string, string, *RunOpts) (*ReservedInvocation, error) {
				reservations++
				inv := &ReservedInvocation{RunID: child, Execute: func(context.Context) (*AgentResult, error) {
					executions++
					return nil, failure
				}}
				switch mode {
				case "write":
					runs.failWrite, runs.failure = 1, failure
				case "reserve":
					return inv, failure
				case "cancel":
					cancel()
				}
				return inv, nil
			})
			rec, err := NewReservedService(runner, runs, nil).RunConfig(ctx, reservedConfig(StrategySequential), "work")
			wantErr, wantExecutions := failure, 0
			if mode == "cancel" {
				wantErr = context.Canceled
			}
			if mode == "execute" {
				wantExecutions = 1
			}
			if !errors.Is(err, wantErr) || rec == nil || rec.Status != StatusFailed || executions != wantExecutions || reservations != 1 {
				t.Fatalf("admission failure: rec=%+v err=%v reserved=%d executed=%d", rec, err, reservations, executions)
			}
			if !slices.Equal(runs.snapshot().AgentRunIDs, []id.AgentRunID{child}) {
				t.Fatalf("reserved identity lost: %+v", runs.snapshot())
			}
		})
	}
}

func TestServiceReservedIdentityCannotChangeOrRepeat(t *testing.T) {
	for _, mode := range []string{"repeat", "mismatch", "nil_result", "nil_invocation", "nil_execute", "nil_id", "wrong_prefix"} {
		t.Run(mode, func(t *testing.T) {
			runs := &executionRunStore{}
			child, other := id.NewAgentRunID(), id.NewAgentRunID()
			executions := 0
			runner := reservingRunnerFunc(func(_ context.Context, _ id.OrchestrationID, name, _ string, _ *RunOpts) (*ReservedInvocation, error) {
				inv := &ReservedInvocation{RunID: child, Execute: func(context.Context) (*AgentResult, error) {
					executions++
					result := &AgentResult{AgentName: name, RunID: child, Output: "answer"}
					if mode == "mismatch" {
						result.RunID = other
					}
					if mode == "nil_result" {
						return nil, nil
					}
					return result, nil
				}}
				switch mode {
				case "nil_invocation":
					return nil, nil
				case "nil_execute":
					inv.Execute = nil
				case "nil_id":
					inv.RunID = id.AgentRunID{}
				case "wrong_prefix":
					inv.RunID = id.NewAgentID()
				}
				return inv, nil
			})
			rec, err := NewReservedService(runner, runs, nil).RunConfig(context.Background(), reservedConfig(StrategySequential), "work")
			if !errors.Is(err, cortex.ErrRunIncomplete) || rec == nil || rec.Status != StatusFailed || executions > 1 {
				t.Fatalf("invalid dispatch accepted: %+v %v executions=%d", rec, err, executions)
			}
			if mode == "repeat" || mode == "mismatch" || mode == "nil_result" || mode == "nil_execute" {
				if !slices.Contains(runs.snapshot().AgentRunIDs, child) {
					t.Fatal("reserved identity lost")
				}
			}
			if mode == "mismatch" && !slices.Contains(runs.snapshot().AgentRunIDs, other) {
				t.Fatal("unexpected returned run identity lost")
			}
			if mode == "repeat" && len(runs.snapshot().AgentRunIDs) != 1 {
				t.Fatal("duplicate dispatch produced duplicate child evidence")
			}
		})
	}
}

func TestServiceReservedExecutionRejectsInvalidTeamID(t *testing.T) {
	for _, team := range []id.OrchestrationID{{}, id.NewAgentID()} {
		runner := reservingRunnerFunc(func(context.Context, id.OrchestrationID, string, string, *RunOpts) (*ReservedInvocation, error) {
			t.Error("invalid team dispatched")
			return nil, nil
		})
		runs := &executionRunStore{}
		rec, err := NewReservedService(runner, runs, nil).RunConfigWithID(context.Background(), reservedConfig(StrategySequential), "work", team)
		if !errors.Is(err, cortex.ErrRunIncomplete) || rec != nil || !runs.snapshot().ID.IsNil() {
			t.Fatalf("invalid team recorded: %+v %v", rec, err)
		}
	}
}

func TestServiceReservedParallelFailureRetainsPendingIdentities(t *testing.T) {
	failure := errors.New("child linkage acknowledgment lost")
	runs := &executionRunStore{failWrite: 1, failure: failure}
	var reserved sync.WaitGroup
	reserved.Add(2)
	runner := reservingRunnerFunc(func(context.Context, id.OrchestrationID, string, string, *RunOpts) (*ReservedInvocation, error) {
		reserved.Done()
		reserved.Wait()
		return &ReservedInvocation{RunID: id.NewAgentRunID(), Execute: func(context.Context) (*AgentResult, error) {
			t.Error("pending child executed after failed linkage")
			return nil, nil
		}}, nil
	})
	rec, err := NewReservedService(runner, runs, nil).RunConfig(context.Background(), reservedConfig(StrategyParallel), "work")
	if !errors.Is(err, failure) || rec == nil || rec.Status != StatusFailed || len(runs.snapshot().AgentRunIDs) != 2 {
		t.Fatalf("parallel admission evidence lost: %+v %v", rec, err)
	}
}

func TestServiceReservedPanicKeepsChildLinkage(t *testing.T) {
	runs := &executionRunStore{}
	child := id.NewAgentRunID()
	runner := reservingRunnerFunc(func(context.Context, id.OrchestrationID, string, string, *RunOpts) (*ReservedInvocation, error) {
		return &ReservedInvocation{RunID: child, Execute: func(context.Context) (*AgentResult, error) {
			panic("execution interrupted")
		}}, nil
	})
	func() {
		defer func() {
			if recover() == nil {
				t.Error("execution did not reach interruption")
			}
		}()
		_, _ = NewReservedService(runner, runs, nil).RunConfig(context.Background(), reservedConfig(StrategySequential), "work")
	}()
	stored := runs.snapshot()
	if stored.Status != StatusRunning || !slices.Equal(stored.AgentRunIDs, []id.AgentRunID{child}) {
		t.Fatalf("interrupted execution lost pending child: %+v", stored)
	}
}
