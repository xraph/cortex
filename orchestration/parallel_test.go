package orchestration

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

func TestParallelRunsAllAndConcatenates(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs = map[string]string{"a": "AA", "b": "BB", "c": "CC"}
	parts := []Participant{{AgentName: "a"}, {AgentName: "b"}, {AgentName: "c"}}
	o := newParallel(runner, parts, Settings{MaxConcurrency: 2})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "go", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := runner.callNames()
	sort.Strings(got)
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("called = %v, want a,b,c", got)
	}
	if len(res.AgentResults) != 3 {
		t.Fatalf("results = %d, want 3", len(res.AgentResults))
	}
	for _, want := range []string{"AA", "BB", "CC"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("output %q missing %q", res.Output, want)
		}
	}
}

func TestParallelFailurePreventsAggregation(t *testing.T) {
	for _, mode := range []string{"error", "embedded", "nil", "empty", "identity"} {
		t.Run(mode, func(t *testing.T) {
			failure := errors.New("worker failed")
			runID := id.NewAgentRunID()
			var aggregatorCalls atomic.Int32
			runner := hierarchyRunnerFunc(func(_ context.Context, name, _ string, _ *RunOpts) (*AgentResult, error) {
				if name == "boss" {
					aggregatorCalls.Add(1)
					return &AgentResult{AgentName: name, Output: "FINAL"}, nil
				}
				out := &AgentResult{AgentName: name, RunID: runID, Output: "partial"}
				switch mode {
				case "error":
					return out, failure
				case "embedded":
					out.Err = failure
				case "nil":
					return nil, nil
				case "empty":
					out.Output = " "
				case "identity":
					out.AgentName = "outsider"
				}
				return out, nil
			})
			parts := []Participant{{AgentName: "worker"}}
			res, err := newParallel(runner, parts, Settings{Aggregator: "boss"}).Run(context.Background(), "act", NewBlackboard(id.NewOrchestrationID(), parts, nil))
			want := failure
			if mode != "error" && mode != "embedded" {
				want = cortex.ErrRunIncomplete
			}
			if !errors.Is(err, want) || !errors.Is(res.Err, want) || res.Output != "" || aggregatorCalls.Load() != 0 {
				t.Fatalf("concealed worker failure: result=%+v error=%v aggregator=%d", res, err, aggregatorCalls.Load())
			}
			if mode != "nil" && res.AgentResults[0].RunID != runID {
				t.Fatal("lost failed run identity")
			}
		})
	}
}

func TestParallelCancellationPreventsQueuedDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	runner := hierarchyRunnerFunc(func(_ context.Context, name, _ string, _ *RunOpts) (*AgentResult, error) {
		calls.Add(1)
		cancel()
		return &AgentResult{AgentName: name, Output: "late"}, nil
	})
	parts := []Participant{{AgentName: "a"}, {AgentName: "b"}, {AgentName: "c"}}
	res, err := newParallel(runner, parts, Settings{MaxConcurrency: 1, Aggregator: "boss"}).Run(ctx, "act", NewBlackboard(id.NewOrchestrationID(), parts, nil))
	if !errors.Is(err, context.Canceled) || !errors.Is(res.Err, context.Canceled) || res.Output != "" || calls.Load() != 1 {
		t.Fatalf("cancellation dispatched more work: result=%+v error=%v calls=%d", res, err, calls.Load())
	}
}

func TestParallelPreservesEveryWorkerFailure(t *testing.T) {
	first, second := errors.New("first failure"), errors.New("second failure")
	runner := hierarchyRunnerFunc(func(_ context.Context, name, _ string, _ *RunOpts) (*AgentResult, error) {
		if name == "a" {
			return nil, first
		}
		return nil, second
	})
	parts := []Participant{{AgentName: "a"}, {AgentName: "b"}}
	res, err := newParallel(runner, parts, Settings{}).Run(context.Background(), "act", NewBlackboard(id.NewOrchestrationID(), parts, nil))
	if !errors.Is(err, first) || !errors.Is(err, second) || !errors.Is(res.Err, first) || !errors.Is(res.Err, second) || res.Output != "" {
		t.Fatalf("lost worker failure: result=%+v error=%v", res, err)
	}
}

func TestParallelAggregatorSynthesizes(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs = map[string]string{"a": "AA", "b": "BB", "boss": "FINAL"}
	parts := []Participant{{AgentName: "a"}, {AgentName: "b"}}
	o := newParallel(runner, parts, Settings{Aggregator: "boss"})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "go", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "FINAL" {
		t.Fatalf("output = %q, want FINAL (aggregator)", res.Output)
	}
}
