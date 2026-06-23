package orchestration

import (
	"context"
	"strings"
	"testing"

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
	o := newHierarchical(runner, "app1", parts, Settings{Manager: "boss"})

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

func TestHierarchicalFallbackWhenPlanInvalid(t *testing.T) {
	runner := newFakeRunner()
	runner.respond = func(agent, input string) string {
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
	o := newHierarchical(runner, "app1", parts, Settings{Manager: "boss"})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "do it", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// both workers must have run despite the invalid plan
	names := strings.Join(runner.callNames(), ",")
	if !strings.Contains(names, "w1") || !strings.Contains(names, "w2") {
		t.Fatalf("fallback did not run all workers: %q", names)
	}
	_ = res
}
