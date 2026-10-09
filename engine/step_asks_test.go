package engine

import (
	"context"
	"testing"

	"github.com/xraph/cortex/id"
)

// A step holds only its own run's asks. Anything else reaching an
// agent_ask on the step's ctx, a host tool calling Dispatch or resuming
// another run, would have its question held by a step that never sends
// it.
func TestAStepHoldsOnlyItsOwnRunsAsks(t *testing.T) {
	runID := id.NewAgentRunID()
	var pending pendingCalls
	ctx := pending.collecting(context.Background(), runID)

	if got := stepPendingCalls(ctx, runID); got != &pending {
		t.Fatal("the step did not find its own run's collector")
	}
	if stepPendingCalls(ctx, id.NewAgentRunID()) != nil {
		t.Fatal("another run's ask was handed to this step")
	}
	if stepPendingCalls(ctx, id.AgentRunID{}) != nil {
		t.Fatal("an ask with no run, as Dispatch makes, was handed to this step")
	}
	if stepPendingCalls(context.Background(), runID) != nil {
		t.Fatal("a ctx outside any step produced a collector")
	}
}
