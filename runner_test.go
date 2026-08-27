package cortex_test

import (
	"context"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/orchestration"
)

// stubRunner satisfies cortex.AgentRunner. The compile-time assertions below
// are the real test: orchestration.AgentRunner must be the same type, so one
// implementation satisfies both names.
type stubRunner struct{}

func (stubRunner) RunAgent(context.Context, string, string, *cortex.RunOpts) (*cortex.AgentResult, error) {
	return &cortex.AgentResult{AgentName: "a", Output: "ok"}, nil
}

var (
	_ cortex.AgentRunner        = stubRunner{}
	_ orchestration.AgentRunner = stubRunner{}
)

func TestAgentRunnerIsSharedAcrossPackages(t *testing.T) {
	var r cortex.AgentRunner = stubRunner{}
	got, err := r.RunAgent(context.Background(), "a", "in", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if got.Output != "ok" {
		t.Fatalf("Output = %q, want %q", got.Output, "ok")
	}
}
