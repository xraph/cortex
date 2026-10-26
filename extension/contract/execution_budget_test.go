package contract

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

func TestDashboardHostBudgetPrecedesPreparationAndCannotExpandClientLimits(t *testing.T) {
	_, service, backend := testService(t)
	ctx, err := service.context(context.Background(), principal("budget"), "run")
	require.NoError(t, err)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "budget-agent", Enabled: true, Model: "old", MaxSteps: 20, MaxTokens: 2000, Metadata: map[string]any{"source": "original"}}
	require.NoError(t, service.deps.Engine.CreateAgent(ctx, ag))
	in := ExecuteInput{ID: ag.ID.String(), Input: "task", Overrides: Overrides{Model: "new", MaxSteps: 7, MaxTokens: 500}}
	_, _, overrides, err := service.prepareExecution(ctx, in)
	require.NoError(t, err)
	require.Nil(t, overrides.Budget)
	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	service.deps.ExecutionBudget = func(_ context.Context, effective *agent.Config) (execution.Budget, error) {
		require.Equal(t, "new", effective.Model)
		require.Equal(t, 7, effective.MaxSteps)
		require.Equal(t, 500, effective.MaxTokens)
		effective.Metadata["source"] = "mutated"
		return execution.Budget{MaxSteps: 3, MaxTokens: 1000, MaxToolCalls: 1, Deadline: deadline.Add(time.Hour)}, nil
	}
	_, _, overrides, err = service.prepareExecution(ctx, in)
	require.NoError(t, err)
	require.Equal(t, execution.Budget{MaxSteps: 3, MaxTokens: 500, MaxToolCalls: 1, Deadline: deadline}, *overrides.Budget)
	current, err := backend.Get(ctx, ag.ID)
	require.NoError(t, err)
	require.Equal(t, "original", current.Metadata["source"])
	service.deps.ExecutionBudget = func(context.Context, *agent.Config) (execution.Budget, error) {
		return execution.Budget{Deadline: time.Now().Add(-time.Second)}, nil
	}
	_, _, _, err = service.prepareExecution(ctx, in)
	require.ErrorIs(t, err, cortex.ErrBudgetExhausted)
}
