package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/store"
)

func testOrchestrationCoordinator(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx, budget, policy, reservation := orchestrationBudgetFixture(t, backend)
	owners, ok := backend.(orchestration.CoordinatorStore)
	require.True(t, ok)
	parent := &orchestration.Run{ID: policy.ExecutionID, Status: orchestration.StatusRunning, ContinuationGeneration: 1, StartedAt: time.Now().UTC(), Input: "work", Strategy: orchestration.StrategySequential}
	require.NoError(t, backend.CreateOrchestrationRun(ctx, parent))
	request := orchestration.CoordinatorRequest{TeamID: parent.ID, Owner: id.New(orchestration.PrefixCoordinator), ExpectedGeneration: 1, Duration: time.Minute, Initial: true}
	claim, err := owners.AcquireOrchestrationCoordinator(ctx, request)
	require.NoError(t, err)
	require.Equal(t, int64(2), claim.Run.ContinuationGeneration)
	require.Equal(t, claim.Run.ContinuationGeneration, claim.Lease.Generation)
	require.Equal(t, request.Owner, claim.Lease.Owner)
	_, err = owners.AcquireOrchestrationCoordinator(ctx, request)
	require.ErrorIs(t, err, cortex.ErrOrchestrationRunConflict)
	renewed, err := owners.RenewOrchestrationCoordinator(ctx, claim.Lease, time.Minute)
	require.NoError(t, err)
	require.Equal(t, claim.Lease.Generation, renewed.Generation)
	require.Greater(t, renewed.Revision, claim.Lease.Revision)
	require.False(t, renewed.ExpiresAt.Before(claim.Lease.ExpiresAt))
	current, err := owners.CheckOrchestrationCoordinator(ctx, renewed)
	require.NoError(t, err)
	require.Equal(t, renewed, current)
	wrong := renewed
	wrong.Owner = id.New(orchestration.PrefixCoordinator)
	_, err = owners.CheckOrchestrationCoordinator(ctx, wrong)
	require.ErrorIs(t, err, cortex.ErrOrchestrationRunConflict)
	_, err = owners.CheckOrchestrationCoordinator(ctxWithScope("foreign"), renewed)
	require.Error(t, err)
	require.ErrorIs(t, backend.UpdateOrchestrationRun(ctx, claim.Run), cortex.ErrOrchestrationRunConflict)
	links, ok := backend.(orchestration.ChildRunStore)
	require.True(t, ok)
	require.ErrorIs(t, links.LinkOrchestrationChild(ctx, parent.ID, id.NewAgentRunID()), cortex.ErrOrchestrationRunConflict)
	_, err = budget.ReserveOrchestrationChild(ctx, reservation)
	require.ErrorIs(t, err, cortex.ErrOrchestrationRunConflict)
	intent := orchestration.InvocationIntent{SchemaVersion: 1, OrchestrationID: parent.ID, Scope: policy.Scope, Slot: "sequential/0", AgentName: "worker", RequestDigest: evidence.Digest([]byte("work"))}
	journal, ok := backend.(orchestration.InvocationStore)
	require.True(t, ok)
	require.ErrorIs(t, journal.BeginOrchestrationInvocation(ctx, intent), cortex.ErrOrchestrationRunConflict)
	require.NoError(t, owners.BeginCoordinatedInvocation(ctx, renewed, intent))
	_, err = owners.ReserveCoordinatedChild(ctx, renewed, reservation)
	require.NoError(t, err)
	linked, err := backend.GetOrchestrationRun(ctx, parent.ID)
	require.NoError(t, err)
	require.Contains(t, linked.AgentRunIDs, reservation.TaskID)
	claim.Run.Status = orchestration.StatusWaiting
	require.NoError(t, owners.FinishOrchestrationCoordinator(ctx, renewed, claim.Run))
	continuations, ok := backend.(orchestration.ContinuationRunStore)
	require.True(t, ok)
	_, err = continuations.ClaimOrchestrationContinuation(ctx, parent.ID)
	require.ErrorIs(t, err, cortex.ErrOrchestrationRunConflict)
	account, err := budget.GetOrchestrationBudgetSnapshot(ctx, parent.ID)
	require.NoError(t, err)
	require.False(t, account.Closed)
	_, err = owners.CheckOrchestrationCoordinator(ctx, renewed)
	require.ErrorIs(t, err, cortex.ErrOrchestrationRunConflict)
	contenders := make(chan orchestration.CoordinatorClaim, 2)
	failures := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			next, acquireErr := owners.AcquireOrchestrationCoordinator(ctx, orchestration.CoordinatorRequest{TeamID: parent.ID, Owner: id.New(orchestration.PrefixCoordinator), ExpectedGeneration: claim.Run.ContinuationGeneration, Duration: time.Minute})
			if acquireErr != nil {
				failures <- acquireErr
			} else {
				contenders <- next
			}
		})
	}
	wg.Wait()
	close(contenders)
	close(failures)
	require.Len(t, contenders, 1)
	require.Len(t, failures, 1)
	require.ErrorIs(t, <-failures, cortex.ErrOrchestrationRunConflict)
	winner := <-contenders
	require.ErrorIs(t, owners.FinishOrchestrationCoordinator(ctx, renewed, claim.Run), cortex.ErrOrchestrationRunConflict)
	winner.Run.Status = orchestration.StatusCompleted
	winner.Run.Output = "done"
	require.NoError(t, owners.FinishOrchestrationCoordinator(ctx, winner.Lease, winner.Run))
	account, err = budget.GetOrchestrationBudgetSnapshot(ctx, parent.ID)
	require.NoError(t, err)
	require.True(t, account.Closed)
	require.Equal(t, 1, account.Reservations)
	_, err = owners.CheckOrchestrationCoordinator(context.Background(), winner.Lease)
	require.ErrorIs(t, err, cortex.ErrNoScope)
}
