package orchestration_test

import (
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func TestOrchestrationRunFields(t *testing.T) {
	r := &orchestration.Run{
		Entity:      cortex.NewEntity(),
		ID:          id.NewOrchestrationID(),
		ConfigID:    id.NewOrchestrationConfigID(),
		AppID:       "app1",
		Strategy:    orchestration.StrategySequential,
		Status:      orchestration.StatusRunning,
		Input:       "hello",
		AgentRunIDs: []id.AgentRunID{id.NewAgentRunID()},
	}
	if r.Status != "running" || r.Strategy != "sequential" {
		t.Fatalf("unexpected run: %+v", r)
	}
	if len(r.AgentRunIDs) != 1 {
		t.Fatalf("agent run ids = %d, want 1", len(r.AgentRunIDs))
	}
}
