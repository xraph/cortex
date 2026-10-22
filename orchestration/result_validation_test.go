package orchestration

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

func TestStrategiesRejectInvalidAgentResults(t *testing.T) {
	cases := []struct {
		name, strategy, failing string
		settings                Settings
	}{
		{"sequential", StrategySequential, "a", Settings{}},
		{"router_decision", StrategyRouter, "dispatcher", Settings{RouterAgent: "dispatcher"}},
		{"router_worker", StrategyRouter, "a", Settings{}},
		{"debater", StrategyDebate, "a", Settings{Judge: "judge"}},
		{"judge", StrategyDebate, "judge", Settings{Judge: "judge"}},
		{"aggregator", StrategyParallel, "boss", Settings{Aggregator: "boss"}},
	}
	for _, tc := range cases {
		for _, mode := range []string{"error", "embedded", "nil", "empty", "identity", "cancelled"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				failure := errors.New("agent failed")
				failedID := id.NewAgentRunID()
				runner := hierarchyRunnerFunc(func(_ context.Context, name, _ string, _ *RunOpts) (*AgentResult, error) {
					if name != tc.failing {
						return &AgentResult{AgentName: name, Output: "valid"}, nil
					}
					out := &AgentResult{AgentName: name, RunID: failedID, Output: "untrusted"}
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
					case "cancelled":
						cancel()
					}
					return out, nil
				})
				parts := []Participant{{AgentName: "a"}, {AgentName: "b"}}
				o, err := Build(tc.strategy, runner, parts, tc.settings)
				if err != nil {
					t.Fatal(err)
				}
				res, err := o.Run(ctx, "act", NewBlackboard(id.NewOrchestrationID(), parts, nil))
				want := cortex.ErrRunIncomplete
				if mode == "error" || mode == "embedded" {
					want = failure
				}
				if mode == "cancelled" {
					want = context.Canceled
				}
				if !errors.Is(err, want) || !errors.Is(res.Err, want) || res.Output != "" {
					t.Fatalf("invalid result published: result=%+v error=%v", res, err)
				}
				if len(res.AgentResults) == 0 {
					t.Fatal("missing failed attempt")
				}
				last := res.AgentResults[len(res.AgentResults)-1]
				if !errors.Is(last.Err, want) || (mode != "nil" && last.RunID != failedID) {
					t.Fatalf("failed attempt lost or downstream dispatched: %+v", res.AgentResults)
				}
			})
		}
	}
}
