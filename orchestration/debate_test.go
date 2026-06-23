package orchestration

import (
	"context"
	"strings"
	"testing"

	"github.com/xraph/cortex/id"
)

func TestDebateRunsRoundsThenJudges(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs = map[string]string{
		"optimist": "PRO",
		"skeptic":  "CON",
		"judge":    "VERDICT",
	}
	parts := []Participant{
		{AgentName: "optimist", Role: "debater"},
		{AgentName: "skeptic", Role: "debater"},
		{AgentName: "judge", Role: "judge"},
	}
	o := newDebate(runner, "app1", parts, Settings{Rounds: 2, Judge: "judge"})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "is X good?", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "VERDICT" {
		t.Fatalf("output = %q, want VERDICT", res.Output)
	}
	// 2 debaters × 2 rounds = 4 debater calls + 1 judge call = 5
	names := runner.callNames()
	if len(names) != 5 {
		t.Fatalf("calls = %d (%v), want 5", len(names), names)
	}
	if names[len(names)-1] != "judge" {
		t.Fatalf("last call = %q, want judge", names[len(names)-1])
	}
}

func TestDebateNoJudgeUsesLastArgument(t *testing.T) {
	runner := newFakeRunner()
	runner.respond = func(agent, _ string) string { return agent + "-arg" }
	parts := []Participant{{AgentName: "a", Role: "debater"}, {AgentName: "b", Role: "debater"}}
	o := newDebate(runner, "app1", parts, Settings{Rounds: 1})

	bb := NewBlackboard(id.NewOrchestrationID(), parts, nil)
	res, err := o.Run(context.Background(), "topic", bb)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Output, "-arg") {
		t.Fatalf("output = %q, want a debater argument", res.Output)
	}
}
