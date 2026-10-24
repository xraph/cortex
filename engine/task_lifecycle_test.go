package engine_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/verification"
)

type lifecyclePlanSource func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error)

func (f lifecyclePlanSource) ListScheduledPlans(ctx context.Context, r coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
	return f(ctx, r)
}

func TestEngineTaskLifecycleRunsGuardedPreparedPipeline(t *testing.T) {
	f := newTaskClaimRuntimeFixture(t, false)
	schedulerConfig := schedulerTaskWorkerConfig(t, f)
	schedulerConfig.Worker = nil
	work := taskWorkerConfig(t, f)
	resolver := work.Resolver
	work.Resolver = taskWorkerResolver(func(ctx context.Context, r coordination.ReadyTaskClaimRequest, a coordination.ReadyTaskClaim) (engine.TaskWork, error) {
		resolved, err := resolver.ResolveTaskWork(ctx, r, a)
		resolved.Dispatch.Claim = a.Acquisition.Claim.Token()
		resolved.Dispatch.ClaimDigest = a.Acquisition.Claim.Digest
		resolved.Dispatch.AdmissionDigest = a.RequestDigest
		return resolved, err
	})
	var discoveries atomic.Int32
	var retry atomic.Bool
	cfg := engine.TaskSchedulingConfig{Worker: work, Service: coordination.TaskServiceConfig{Scheduler: schedulerConfig, Interval: time.Second, Discovery: lifecyclePlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
		if discoveries.Add(1) > 1 && !retry.Load() {
			return coordination.TaskPlanPage{}, nil
		}
		return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: f.request.Acquisition.Reservation.Scope, PlanID: f.request.Acquisition.Claim.PlanID}}}, nil
	})}}
	require.NoError(t, engine.WithTaskScheduling(cfg)(f.engine))
	startup, cancel := context.WithCancel(f.f.ctx)
	require.NoError(t, f.engine.Start(startup))
	cancel()
	t.Cleanup(func() { require.NoError(t, f.engine.Stop(context.Background())) })
	require.Eventually(t, func() bool {
		result, err := f.f.storage.GetRun(f.f.ctx, f.dispatch.RunID)
		return err == nil && result.VerificationStatus == verification.StatusPassed
	}, 15*time.Second, 10*time.Millisecond)
	require.NoError(t, f.engine.Stop(context.Background()))
	status, configured := f.engine.TaskSchedulingStatus()
	require.True(t, configured)
	require.False(t, status.Running)
	require.False(t, status.Stopping)
	require.Zero(t, status.Active)
	require.Zero(t, status.Failures)
	result, err := f.f.storage.GetRun(f.f.ctx, f.dispatch.RunID)
	require.NoError(t, err)
	require.Equal(t, "done", result.Output)
	require.Equal(t, 1, f.client.calls)
	retry.Store(true)
	require.NoError(t, f.engine.Start(f.f.ctx))
	require.Eventually(t, func() bool { status, _ := f.engine.TaskSchedulingStatus(); return status.Failures > 0 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, f.engine.Stop(context.Background()))
	require.Equal(t, 1, f.client.calls)
	budget, err := f.f.storage.GetBudgetSnapshot(f.f.ctx, f.request.Acquisition.Claim.PlanID)
	require.NoError(t, err)
	require.Equal(t, f.request.Acquisition.Reservation.Allocation, budget.Charged)
}

func TestEngineTaskLifecycleRejectsMissingRuntimeGuards(t *testing.T) {
	f := newTaskClaimRuntimeFixture(t, false)
	cfg := schedulerTaskWorkerConfig(t, f)
	cfg.Worker = nil
	option := engine.WithTaskScheduling(engine.TaskSchedulingConfig{Worker: taskWorkerConfig(t, f), Service: coordination.TaskServiceConfig{Scheduler: cfg, Discovery: lifecyclePlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
		return coordination.TaskPlanPage{}, nil
	})}})
	_, err := engine.New(option)
	require.Error(t, err)
	// A late option is supported before Start, but Start repeats guard validation.
	unguarded, err := engine.New()
	require.NoError(t, err)
	require.NoError(t, option(unguarded))
	require.Error(t, unguarded.Start(context.Background()))
	status, configured := unguarded.TaskSchedulingStatus()
	require.True(t, configured)
	require.False(t, status.Running)
}
