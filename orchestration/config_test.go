package orchestration_test

import (
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func TestOrchestrationConfigFields(t *testing.T) {
	c := &orchestration.OrchestrationConfig{
		Entity:   cortex.NewEntity(),
		ID:       id.NewOrchestrationConfigID(),
		Name:     "research-team",
		AppID:    "app1",
		Strategy: orchestration.StrategyDebate,
		Participants: []orchestration.Participant{
			{AgentName: "optimist", Role: "debater"},
			{AgentName: "skeptic", Role: "debater"},
			{AgentName: "judge", Role: "judge"},
		},
		Settings: orchestration.Settings{Rounds: 2, Judge: "judge"},
	}
	if c.Name != "research-team" || c.Strategy != "debate" {
		t.Fatalf("unexpected config: %+v", c)
	}
	if len(c.Participants) != 3 || c.Settings.Rounds != 2 {
		t.Fatalf("unexpected participants/settings: %+v", c)
	}
}
