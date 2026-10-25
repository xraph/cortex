package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/store"
)

func testOrchestrationExpiry(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx, budget, policy, _ := orchestrationBudgetFixture(t, backend)
	expiry, ok := backend.(orchestration.BudgetExpiryStore)
	require.True(t, ok)
	expired, err := expiry.ExpireOrchestrationBudget(ctx, policy.ExecutionID)
	require.NoError(t, err)
	require.False(t, expired)
	snapshot, err := budget.GetOrchestrationBudgetSnapshot(ctx, policy.ExecutionID)
	require.NoError(t, err)
	require.False(t, snapshot.Closed)
	policy.ExecutionID = id.NewOrchestrationID()
	policy.Deadline = time.Now().Add(-time.Minute)
	require.NoError(t, budget.CreateOrchestrationBudget(ctx, policy))
	for range 2 {
		expired, err = expiry.ExpireOrchestrationBudget(ctx, policy.ExecutionID)
		require.NoError(t, err)
		require.True(t, expired)
	}
	parent := &orchestration.Run{ID: policy.ExecutionID, Status: orchestration.StatusWaiting, ContinuationGeneration: 1, StartedAt: time.Now()}
	require.NoError(t, backend.CreateOrchestrationRun(ctx, parent))
	expiryStore, capable := backend.(orchestration.ExpiryStore)
	require.True(t, capable)
	expirer, err := orchestration.NewExpirer(expiryStore)
	require.NoError(t, err)
	require.NoError(t, expirer.Expire(ctx, policy.ExecutionID))
	failed, err := backend.GetOrchestrationRun(ctx, policy.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, orchestration.StatusFailed, failed.Status)
	require.ErrorIs(t, backend.UpdateOrchestrationRun(ctx, parent), cortex.ErrOrchestrationRunClosed)
	snapshot, err = budget.GetOrchestrationBudgetSnapshot(ctx, policy.ExecutionID)
	require.NoError(t, err)
	require.True(t, snapshot.Closed)
	_, err = expiry.ExpireOrchestrationBudget(context.Background(), policy.ExecutionID)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	_, err = expiry.ExpireOrchestrationBudget(ctxWithScope("other"), policy.ExecutionID)
	require.ErrorIs(t, err, execution.ErrReservationNotFound)
	discovery, ok := backend.(orchestration.BudgetDiscoveryStore)
	require.True(t, ok)
	page, err := discovery.ListOrchestrationBudgets(ctx, orchestration.BudgetListRequest{Limit: 1})
	require.NoError(t, err)
	require.Len(t, page, 1)
	next, err := discovery.ListOrchestrationBudgets(ctx, orchestration.BudgetListRequest{Limit: 1, After: page[0].ExecutionID})
	require.NoError(t, err)
	require.Len(t, next, 1)
	require.NotEqual(t, page[0].ExecutionID, next[0].ExecutionID)
	end, err := discovery.ListOrchestrationBudgets(ctx, orchestration.BudgetListRequest{Limit: 1, After: next[0].ExecutionID})
	require.NoError(t, err)
	require.Empty(t, end)
	require.True(t, next[0].Deadline.Before(time.Now()), "closed budgets stay discoverable for parent cleanup retries")
	_, err = discovery.ListOrchestrationBudgets(context.Background(), orchestration.BudgetListRequest{Limit: 1})
	require.ErrorIs(t, err, cortex.ErrNoScope)
	other, err := discovery.ListOrchestrationBudgets(ctxWithScope("other"), orchestration.BudgetListRequest{Limit: 1})
	require.NoError(t, err)
	require.Empty(t, other)
	for _, request := range []orchestration.BudgetListRequest{{}, {Limit: 257}, {Limit: 1, After: id.NewAgentID()}} {
		_, err = discovery.ListOrchestrationBudgets(ctx, request)
		require.Error(t, err)
	}
}
