package orchestration

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"

	"strings"
	"testing"

	"github.com/xraph/cortex"

	"github.com/xraph/cortex/id"
)

func TestHierarchicalDelegatesPerPlan(t *testing.T) {
	runner := newFakeRunner()
	plan := `[{"agent":"researcher","task":"gather facts"},{"agent":"writer","task":"compose"}]`
	calls := 0
	runner.respond = func(agent, input string) string {
		switch agent {
		case "boss":
			calls++
			if calls == 1 {
				return plan // first call: the delegation plan
			}
			return "SYNTHESIS" // second call: synthesis
		case "researcher":
			return "FACTS"
		case "writer":
			return "ESSAY"
		}
		return input
	}
	parts := []Participant{
		{AgentName: "boss", Role: "manager"},
		{AgentName: "researcher", Role: "worker"},
		{AgentName: "writer", Role: "worker"},
	}
	o := newHierarchical(runner, parts, Settings{Manager: "boss"})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "write a report", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "SYNTHESIS" {
		t.Fatalf("output = %q, want SYNTHESIS", res.Output)
	}
	names := strings.Join(runner.callNames(), ",")
	for _, want := range []string{"boss", "researcher", "writer"} {
		if !strings.Contains(names, want) {
			t.Fatalf("calls %q missing %q", names, want)
		}
	}
	// at least one manager→worker handoff recorded
	if len(res.Handoffs) == 0 {
		t.Fatalf("expected handoffs, got none")
	}
}

func TestMalformedPlanDispatchesNoWorkers(t *testing.T) {
	runner := newFakeRunner()
	runner.respond = func(agent, _ string) string {
		if agent == "boss" {
			return "not json"
		}
		return agent + "-done"
	}
	parts := []Participant{
		{AgentName: "boss", Role: "manager"},
		{AgentName: "w1", Role: "worker"},
		{AgentName: "w2", Role: "worker"},
	}
	o := newHierarchical(runner, parts, Settings{Manager: "boss"})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "do it", bb)
	if err == nil || res.Err == nil {
		t.Fatal("invalid plan must fail")
	}
	if names := strings.Join(runner.callNames(), ","); names != "boss" {
		t.Fatalf("invalid plan dispatched workers: %s", names)
	}
}

func TestUnknownWorkerRejected(t *testing.T) {
	assertRejectedPlan(t, `[{"agent":"w1","task":"ok"},{"agent":"outsider","task":"act"}]`)
}

func TestPartialPlanDoesNotDropRequiredAssignment(t *testing.T) {
	assertRejectedPlan(t, `[{"agent":"w1","task":"ok"}]`)
}

func assertRejectedPlan(t *testing.T, plan string) {
	t.Helper()
	runner := newFakeRunner()
	runner.outputs["boss"] = plan
	parts := []Participant{{AgentName: "boss"}, {AgentName: "w1"}, {AgentName: "w2"}}
	res, err := newHierarchical(runner, parts, Settings{Manager: "boss"}).Run(context.Background(), "act", NewBlackboard(id.NewOrchestrationID(), parts, nil))
	if err == nil || res.Err == nil || len(runner.callNames()) != 1 {
		t.Fatalf("unsafe plan accepted: result=%+v error=%v calls=%v", res, err, runner.callNames())
	}
}

type hierarchyRunnerFunc func(context.Context, string, string, *RunOpts) (*AgentResult, error)

func (f hierarchyRunnerFunc) RunAgent(ctx context.Context, name, input string, opts *RunOpts) (*AgentResult, error) {
	return f(ctx, name, input, opts)
}

func TestWorkerFailureCannotYieldUnqualifiedSuccess(t *testing.T) {
	for _, mode := range []string{"error", "embedded", "nil", "empty", "identity"} {
		t.Run(mode, func(t *testing.T) {
			managerCalls := 0
			failure := errors.New("worker failed")
			runner := hierarchyRunnerFunc(func(_ context.Context, name, _ string, _ *RunOpts) (*AgentResult, error) {
				if name == "boss" {
					managerCalls++
					return &AgentResult{AgentName: name, Output: `[{"agent":"w1","task":"act"}]`}, nil
				}
				switch mode {
				case "error":
					return &AgentResult{AgentName: name, RunID: id.NewAgentRunID()}, failure
				case "embedded":
					return &AgentResult{AgentName: name, Err: failure}, nil
				case "nil":
					return nil, nil
				case "empty":
					return &AgentResult{AgentName: name}, nil
				default:
					return &AgentResult{AgentName: "outsider", Output: "done"}, nil
				}
			})
			parts := []Participant{{AgentName: "boss"}, {AgentName: "w1"}}
			res, err := newHierarchical(runner, parts, Settings{}).Run(context.Background(), "act", NewBlackboard(id.NewOrchestrationID(), parts, nil))
			if err == nil || res.Err == nil || res.Output != "" || managerCalls != 1 {
				t.Fatalf("concealed worker failure: %+v %v manager=%d", res, err, managerCalls)
			}
			if mode == "error" && res.AgentResults[1].RunID.IsNil() {
				t.Fatal("lost failed run identity")
			}
		})
	}
}

func TestPlanRepairConsumesBudget(t *testing.T) {
	for _, repairWorks := range []bool{false, true} {
		t.Run(strconv.FormatBool(repairWorks), func(t *testing.T) {
			managerCalls, workerCalls := 0, 0
			runner := hierarchyRunnerFunc(func(_ context.Context, name, input string, _ *RunOpts) (*AgentResult, error) {
				out := "done"
				if name == "boss" {
					managerCalls++
					if workerCalls == 0 {
						out = "invalid"
						if managerCalls == 2 {
							if !strings.Contains(input, `"validation_error"`) {
								t.Fatal("repair lacks structured validation error")
							}
							if repairWorks {
								out = `[{"agent":"w1","task":"act"}]`
							}
						}
					}
				} else {
					workerCalls++
				}
				return &AgentResult{AgentName: name, RunID: id.NewAgentRunID(), Output: out}, nil
			})
			parts := []Participant{{AgentName: "boss"}, {AgentName: "w1"}}
			res, err := newHierarchical(runner, parts, Settings{PlanRepairAttempts: 1}).Run(context.Background(), "act", NewBlackboard(id.NewOrchestrationID(), parts, nil))
			if repairWorks {
				if err != nil || managerCalls != 3 || workerCalls != 1 || len(res.AgentResults) != 4 {
					t.Fatalf("repair calls not accounted: %+v %v manager=%d workers=%d", res, err, managerCalls, workerCalls)
				}
			} else if err == nil || managerCalls != 2 || workerCalls != 0 || len(res.AgentResults) != 2 {
				t.Fatalf("repair exceeded cap: %+v %v manager=%d workers=%d", res, err, managerCalls, workerCalls)
			}
			for _, r := range res.AgentResults {
				if r.RunID.IsNil() {
					t.Fatal("missing charged run identity")
				}
			}
		})
	}
}

func TestHierarchicalCancellationStopsQueuedWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var workerCalls atomic.Int32
	runner := hierarchyRunnerFunc(func(_ context.Context, name, _ string, _ *RunOpts) (*AgentResult, error) {
		if name == "boss" {
			return &AgentResult{AgentName: name, Output: `[{"agent":"w1","task":"one"},{"agent":"w2","task":"two"},{"agent":"w3","task":"three"}]`}, nil
		}
		workerCalls.Add(1)
		cancel()
		return &AgentResult{AgentName: name, Output: "partial"}, nil
	})
	parts := []Participant{{AgentName: "boss"}, {AgentName: "w1"}, {AgentName: "w2"}, {AgentName: "w3"}}
	res, err := newHierarchical(runner, parts, Settings{MaxConcurrency: 1}).Run(ctx, "act", NewBlackboard(id.NewOrchestrationID(), parts, nil))
	if !errors.Is(err, context.Canceled) || workerCalls.Load() != 1 || res.Output != "" {
		t.Fatalf("cancellation: %+v %v calls=%d", res, err, workerCalls.Load())
	}
}

func TestHierarchicalRejectsInvalidRosterBeforeManager(t *testing.T) {
	for _, tc := range []struct {
		name     string
		parts    []Participant
		settings Settings
	}{
		{"empty", nil, Settings{}},
		{"manager only", []Participant{{AgentName: "boss"}}, Settings{}},
		{"unknown manager", []Participant{{AgentName: "boss"}, {AgentName: "w1"}}, Settings{Manager: "outsider"}},
		{"duplicate", []Participant{{AgentName: "boss"}, {AgentName: "w1"}, {AgentName: "w1"}}, Settings{}},
		{"blank", []Participant{{AgentName: "boss"}, {AgentName: " "}}, Settings{}},
		{"negative repair", []Participant{{AgentName: "boss"}, {AgentName: "w1"}}, Settings{PlanRepairAttempts: -1}},
		{"excess repair", []Participant{{AgentName: "boss"}, {AgentName: "w1"}}, Settings{PlanRepairAttempts: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := newFakeRunner()
			_, err := newHierarchical(runner, tc.parts, tc.settings).Run(context.Background(), "act", NewBlackboard(id.NewOrchestrationID(), tc.parts, nil))
			if !errors.Is(err, cortex.ErrInvalidDelegationPlan) || len(runner.callNames()) != 0 {
				t.Fatalf("roster reached manager: %v %v", err, runner.callNames())
			}
		})
	}
}

func TestHierarchicalRejectsMissingManagerResults(t *testing.T) {
	for _, phase := range []string{"plan", "synthesis"} {
		t.Run(phase, func(t *testing.T) {
			managerCalls := 0
			runner := hierarchyRunnerFunc(func(_ context.Context, name, _ string, _ *RunOpts) (*AgentResult, error) {
				if name == "boss" {
					managerCalls++
					if phase == "plan" || managerCalls == 2 {
						return nil, nil
					}
					return &AgentResult{AgentName: name, Output: `[{"agent":"w1","task":"act"}]`}, nil
				}
				return &AgentResult{AgentName: name, Output: "done"}, nil
			})
			parts := []Participant{{AgentName: "boss"}, {AgentName: "w1"}}
			res, err := newHierarchical(runner, parts, Settings{PlanRepairAttempts: 1}).Run(context.Background(), "act", NewBlackboard(id.NewOrchestrationID(), parts, nil))
			if !errors.Is(err, cortex.ErrRunIncomplete) || res.Output != "" {
				t.Fatalf("missing result: %+v %v", res, err)
			}
		})
	}
}

func TestHierarchicalRejectsRepairSettingsAtBuild(t *testing.T) {
	for _, attempts := range []int{-1, 2, 100} {
		_, err := Build(StrategyHierarchical, newFakeRunner(), nil, Settings{PlanRepairAttempts: attempts})
		if !errors.Is(err, cortex.ErrInvalidDelegationPlan) {
			t.Fatalf("attempts %d: %v", attempts, err)
		}
	}
}

func TestPlanRepairHonorsRunnerBudgetFailure(t *testing.T) {
	calls := 0
	runner := hierarchyRunnerFunc(func(_ context.Context, name, _ string, _ *RunOpts) (*AgentResult, error) {
		calls++
		if name != "boss" {
			t.Fatal("worker ran after repair budget failure")
		}
		result := &AgentResult{AgentName: name, RunID: id.NewAgentRunID(), Output: "invalid"}
		if calls == 2 {
			return result, cortex.ErrBudgetExhausted
		}
		return result, nil
	})
	parts := []Participant{{AgentName: "boss"}, {AgentName: "w1"}}
	res, err := newHierarchical(runner, parts, Settings{PlanRepairAttempts: 1}).Run(context.Background(), "act", NewBlackboard(id.NewOrchestrationID(), parts, nil))
	if !errors.Is(err, cortex.ErrBudgetExhausted) || calls != 2 || len(res.AgentResults) != 2 || res.AgentResults[1].RunID.IsNil() {
		t.Fatalf("budget failure lost: %+v %v calls=%d", res, err, calls)
	}
}
