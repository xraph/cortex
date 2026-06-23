package orchestration

import (
	"context"
	"strings"
	"testing"

	"github.com/xraph/cortex/id"
)

func TestSequentialRunsInOrderAndChains(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs = map[string]string{"writer": "DRAFT", "editor": "EDITED"}
	parts := []Participant{{AgentName: "writer"}, {AgentName: "editor"}}
	o := newSequential(runner, "app1", parts, Settings{})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "start", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := runner.callNames(); len(got) != 2 || got[0] != "writer" || got[1] != "editor" {
		t.Fatalf("call order = %v, want [writer editor]", got)
	}
	if res.Output != "EDITED" {
		t.Fatalf("output = %q, want EDITED", res.Output)
	}
	if len(res.AgentResults) != 2 {
		t.Fatalf("agent results = %d, want 2", len(res.AgentResults))
	}
	if len(res.Handoffs) != 1 || res.Handoffs[0].From != "writer" || res.Handoffs[0].To != "editor" {
		t.Fatalf("handoffs = %+v, want one writer→editor", res.Handoffs)
	}
}

func TestSequentialSecondAgentSeesFirstOutput(t *testing.T) {
	runner := newFakeRunner()
	runner.respond = func(agent, input string) string { return agent + ":" + input }
	parts := []Participant{{AgentName: "a"}, {AgentName: "b"}}
	o := newSequential(runner, "app1", parts, Settings{})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	_, err := o.Run(context.Background(), "X", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// b's input must contain the first agent's response via the blackboard snapshot.
	calls := runner.calls
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	// Check that the snapshot section contains output from agent a
	if !strings.Contains(calls[1].Input, "[a]:") {
		t.Fatalf("second agent input %q does not show first agent's work", calls[1].Input)
	}
}

func TestSequentialFirstAgentSeesRoster(t *testing.T) {
	runner := newFakeRunner()
	parts := []Participant{{AgentName: "writer", Role: "author"}, {AgentName: "editor", Role: "critic"}}
	o := newSequential(runner, "app1", parts, Settings{})
	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	if _, err := o.Run(context.Background(), "start", bb); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// first agent's input must contain the roster (e.g. the other participant's name)
	first := runner.calls[0].Input
	if !strings.Contains(first, "editor") {
		t.Fatalf("first agent input %q lacks roster awareness", first)
	}
}
