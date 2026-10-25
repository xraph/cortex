package storetest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/store"
)

type orchestrationReserverFunc func(context.Context, id.OrchestrationID, string, string, *orchestration.RunOpts) (*orchestration.ReservedInvocation, error)

func (f orchestrationReserverFunc) ReserveAgent(ctx context.Context, team id.OrchestrationID, name, input string, opts *orchestration.RunOpts) (*orchestration.ReservedInvocation, error) {
	return f(ctx, team, name, input, opts)
}

func testOrchestrationReservedExecution(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, budgets, policy, request := orchestrationBudgetFixture(t, s)
	var calls atomic.Int64
	runner := orchestrationReserverFunc(func(callCtx context.Context, team id.OrchestrationID, name, _ string, _ *orchestration.RunOpts) (*orchestration.ReservedInvocation, error) {
		if team != policy.ExecutionID || name != request.OwnerRef {
			t.Error("reserved account binding changed")
		}
		if _, err := budgets.ReserveOrchestrationChild(callCtx, request); err != nil {
			return nil, err
		}
		return &orchestration.ReservedInvocation{RunID: request.TaskID, Execute: func(execCtx context.Context) (*orchestration.AgentResult, error) {
			calls.Add(1)
			stored, err := s.GetOrchestrationRun(execCtx, team)
			if err != nil || stored == nil || len(stored.AgentRunIDs) != 1 || stored.AgentRunIDs[0] != request.TaskID || stored.Status != orchestration.StatusRunning {
				t.Errorf("team linkage unavailable during execution: %+v %v", stored, err)
			}
			allocation, err := budgets.GetReservation(execCtx, request.ID)
			if err != nil || allocation.Request.TaskID != request.TaskID || allocation.Request.PlanID != team {
				t.Errorf("child budget unavailable during execution: %+v %v", allocation, err)
			}
			return &orchestration.AgentResult{AgentName: name, RunID: request.TaskID, Output: "answer"}, nil
		}}, nil
	})
	cfg := orchestration.Config{ID: id.NewOrchestrationConfigID(), Name: "reserved-team", Strategy: orchestration.StrategySequential, Participants: []orchestration.Participant{{AgentName: "worker"}}}
	require.NoError(t, s.CreateOrchestration(ctx, &cfg))
	svc := orchestration.NewReservedService(runner, s, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, err := svc.RunConfigWithID(ctx, cfg, "work", policy.ExecutionID)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	wins := 0
	for err := range errs {
		if err == nil {
			wins++
		}
	}
	require.Equal(t, 1, wins)
	require.EqualValues(t, 1, calls.Load(), "duplicate creation must not re-execute a child")
	_, err := svc.RunConfigWithID(ctx, cfg, "retry", policy.ExecutionID)
	require.Error(t, err)
	require.EqualValues(t, 1, calls.Load())
	stored, err := s.GetOrchestrationRun(ctx, policy.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, orchestration.StatusCompleted, stored.Status)
	require.Equal(t, "work", stored.Input)
	require.Equal(t, []id.AgentRunID{request.TaskID}, stored.AgentRunIDs)
	snapshot, err := budgets.GetOrchestrationBudgetSnapshot(ctx, policy.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, 1, snapshot.Reservations)
	require.Equal(t, request.Allocation, snapshot.Charged, "unverified usage remains fully charged")
}
