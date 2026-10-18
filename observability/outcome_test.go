package observability

import (
	"context"
	"testing"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/plugin"
	"github.com/xraph/cortex/run"
)

func TestIncompleteOutcomeDoesNotCountAsCompleted(t *testing.T) {
	metrics := NewMetricsExtension()
	registry := plugin.NewRegistry(nil)
	registry.Register(metrics)
	registry.EmitRunTerminated(context.Background(), id.NewAgentID(), id.NewAgentRunID(), run.TerminalSummary{State: run.StateIncomplete, StopReason: run.StopPolicyUnavailable})
	if metrics.RunIncompleteCount.Value() != 1 || metrics.RunCompletedCount.Value() != 0 || metrics.RunPolicyFailureCount.Value() != 1 {
		t.Fatal("incorrect outcome counts")
	}
}
