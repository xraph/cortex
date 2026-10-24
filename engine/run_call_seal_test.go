package engine_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/execution"
)

type runtimeSealFence func(context.Context, execution.RunCallSealRequest, func(execution.RunCallSealRequest) error) error

func (f runtimeSealFence) WithRunCallSeal(ctx context.Context, r execution.RunCallSealRequest, next func(execution.RunCallSealRequest) error) error {
	return f(ctx, r, next)
}

type runtimeSealAuthority func(context.Context, execution.RunCallSealRequest) error

func (f runtimeSealAuthority) AuthorizeRunCallSeal(ctx context.Context, r execution.RunCallSealRequest) error {
	return f(ctx, r)
}

func TestTaskWorkerSealedJournalPreventsProviderDispatch(t *testing.T) {
	f := preparedTaskClaimFixture(t)
	worker, err := f.engine.NewTaskWorker(taskWorkerConfig(t, f))
	require.NoError(t, err)
	sealer, err := f.engine.NewRunCallSealer(runtimeSealFence(func(_ context.Context, r execution.RunCallSealRequest, next func(execution.RunCallSealRequest) error) error {
		return next(r)
	}), runtimeSealAuthority(func(context.Context, execution.RunCallSealRequest) error { return nil }))
	require.NoError(t, err)
	request := execution.RunCallSealRequest{SchemaVersion: 1, Scope: f.request.Acquisition.Reservation.Scope, RunID: f.dispatch.RunID, Actor: "fixture recovery service", Reason: "stop delayed admission"}
	receipt, err := sealer.Seal(f.f.ctx, request)
	require.NoError(t, err)
	require.NoError(t, receipt.Validate())
	err = worker.RunClaimedTask(f.f.ctx, f.request, f.ready)
	require.EqualError(t, err, "llm complete: cortex: provider request failed")
	require.Zero(t, f.client.calls)
	calls, err := f.f.storage.ListRunCalls(f.f.ctx, execution.RunCallListRequest{RunID: f.dispatch.RunID, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, calls)
	reopened := openSQLiteStoreForTest(t, f.f.path)
	got, err := reopened.GetRunCallSeal(f.f.ctx, f.dispatch.RunID)
	require.NoError(t, err)
	require.Equal(t, &receipt, got)
	budget, err := reopened.GetBudgetSnapshot(f.f.ctx, f.request.Acquisition.Claim.PlanID)
	require.NoError(t, err)
	require.Equal(t, f.request.Acquisition.Reservation.Allocation, budget.Charged)
}
