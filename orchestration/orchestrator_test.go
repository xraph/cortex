package orchestration_test

import (
	"testing"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func TestStrategyConstants(t *testing.T) {
	cases := map[string]string{
		orchestration.StrategySequential:   "sequential",
		orchestration.StrategyParallel:     "parallel",
		orchestration.StrategyRouter:       "router",
		orchestration.StrategyHierarchical: "hierarchical",
		orchestration.StrategyDebate:       "debate",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("strategy constant = %q, want %q", got, want)
		}
	}
}

func TestResultConstruction(t *testing.T) {
	r := &orchestration.Result{
		OrchestrationID: id.NewOrchestrationID(),
		Strategy:        orchestration.StrategySequential,
		Output:          "done",
		AgentResults: []orchestration.AgentResult{
			{AgentName: "writer", Output: "draft"},
		},
		Handoffs: []orchestration.Handoff{{From: "writer", To: "editor", Payload: "draft"}},
	}
	if r.Output != "done" || len(r.AgentResults) != 1 || len(r.Handoffs) != 1 {
		t.Fatalf("unexpected result: %+v", r)
	}
}
