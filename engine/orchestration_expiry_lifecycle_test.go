package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
)

func TestEngineOrchestrationExpiryClosesWaitingTeam(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	refreshPreparedTeam(t, &f, engine.WithExternalTool(llm.Tool{Name: "probe"}))
	f.policy.Deadline = time.Now().Add(2 * time.Second)
	f.client.complete = func(context.Context, *llm.Request, int) (*llm.Response, error) {
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "probe-call", Name: "probe", Arguments: "{}"}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	team, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.Error(t, err)
	require.NotNil(t, team)
	require.Equal(t, orchestration.StatusWaiting, team.Status)
	before, err := f.s.GetOrchestrationBudgetSnapshot(f.ctx, team.ID)
	require.NoError(t, err)
	expirer, err := orchestration.NewExpirer(f.s)
	require.NoError(t, err)
	source, err := coordination.NewScopedOrchestrationSource(f.s, []cortex.Scope{f.policy.Scope}, func(context.Context, cortex.Scope) error { return nil })
	require.NoError(t, err)
	require.NoError(t, engine.WithOrchestrationExpiry(coordination.OrchestrationExpiryConfig{Discovery: source, Expirer: expirer, Interval: 10 * time.Millisecond})(f.e))
	require.NoError(t, f.e.Start(f.ctx))
	t.Cleanup(func() { require.NoError(t, f.e.Stop(context.Background())) })
	require.Eventually(t, func() bool {
		current, readErr := f.s.GetOrchestrationRun(f.ctx, team.ID)
		return readErr == nil && current.Status == orchestration.StatusFailed
	}, 10*time.Second, 20*time.Millisecond)
	require.NoError(t, f.e.Stop(context.Background()))
	after, err := f.s.GetOrchestrationBudgetSnapshot(f.ctx, team.ID)
	require.NoError(t, err)
	require.True(t, after.Closed)
	require.Equal(t, before.Charged, after.Charged)
	require.Equal(t, before.Reservations, after.Reservations)
	child, err := f.s.GetRun(f.ctx, team.AgentRunIDs[0])
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, child.State, "parent expiry must not invent a child outcome")
	_, err = f.e.Resume(f.ctx, child.ID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "probe-call", Content: "result"}}})
	require.Error(t, err)
	require.Equal(t, 1, f.client.calls)
	status, configured := f.e.OrchestrationExpiryStatus()
	require.True(t, configured)
	require.False(t, status.Running)
	require.Zero(t, status.Active)
}
