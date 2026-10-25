package storetest

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

func testManagedTerminalTaskSettlement(t *testing.T, backend store.Store, driver assurance.Driver) {
	f, _ := terminalUsageFixture(t, backend, driver, run.StateFailed)
	testManagedClosedTaskSettlement(t, backend, driver, f, false)
}

func testManagedRejectedTaskSettlement(t *testing.T, backend store.Store, driver assurance.Driver) {
	f, request := rejectedUsageFixture(t, backend, driver)
	_, err := f.repo.CloseTaskUsage(f.ctx, request)
	require.NoError(t, err)
	testManagedClosedTaskSettlement(t, backend, driver, f, true)
}

func testManagedClosedTaskSettlement(t *testing.T, backend store.Store, driver assurance.Driver, f taskRecoveryFixture, rejected bool) {
	claim, err := f.repo.GetTaskClaim(f.ctx, f.request.Claim.PlanID, f.request.Claim.TaskID)
	require.NoError(t, err)
	original, err := backend.GetRun(f.ctx, claim.RunID)
	require.NoError(t, err)
	observed := &settlementLifecycleStore{TaskSettlementStore: f.repo, loseFirstAck: true}
	authority := settlementLifecycleAuthority{}
	var resolutions atomic.Int32
	cfg := coordination.TaskSettlementServiceConfig{Store: observed, ClosureActor: "accounting", Interval: 20 * time.Millisecond,
		Discovery: adoptionPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: claim.Scope, PlanID: claim.PlanID}}}, nil
		}),
		AuthorizeScan: func(context.Context, coordination.Claim, execution.RunCallSeal) error { return nil }, ClosureFence: authority, ClosureAuthorizer: authority, Fence: authority, Authorizer: authority,
		Resolver: settlementLifecycleResolver(func(_ context.Context, snapshot coordination.TaskUsageSnapshot, closed coordination.TaskUsageClosure, _ execution.Reservation) (*coordination.TaskUsageSettlementRequest, error) {
			resolutions.Add(1)
			require.Equal(t, claim, snapshot.Claim)
			if rejected {
				require.NotNil(t, closed.Request.Rejection)
				require.Equal(t, original.Revision, closed.Request.Rejection.RunRevision)
			} else {
				require.NotNil(t, closed.Request.Termination)
				require.Equal(t, original.Revision, closed.Request.Termination.RunRevision)
			}
			r := terminalSettlementRequest(t, f, closed.Request, closed)
			return &r, nil
		}),
	}
	// Opt-in traversal must reach recovering claims without changing acceptance discovery.
	cfg.UnsuccessfulRuns = backend
	eng, err := engine.New(engine.WithStore(backend), engine.WithTaskSettlement(cfg), engine.WithSuspensionTTL(0))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, eng.Stop(context.Background())) })
	require.NoError(t, eng.Start(f.ctx))
	require.Eventually(t, func() bool { return observed.attempts.Load() >= 2 }, 15*time.Second, 10*time.Millisecond)
	require.NoError(t, eng.Stop(context.Background()))
	require.EqualValues(t, 1, resolutions.Load())
	account, err := f.repo.GetBudgetSnapshot(f.ctx, claim.PlanID)
	require.NoError(t, err)
	require.EqualValues(t, 18, account.Charged.Tokens)
	require.EqualValues(t, 8, account.Charged.USDMicros)
	after, err := f.repo.GetTaskClaim(f.ctx, claim.PlanID, claim.TaskID)
	require.NoError(t, err)
	require.Equal(t, claim, after)
	current, err := backend.GetRun(f.ctx, claim.RunID)
	require.NoError(t, err)
	require.Equal(t, original, current)
	f.clock.advance(time.Hour)
	reopened := assurance.New(driver).WithClock(f.clock)
	restartedStore := &settlementLifecycleStore{TaskSettlementStore: reopened}
	cfg.Store = restartedStore
	restarted, err := engine.New(engine.WithStore(backend), engine.WithTaskSettlement(cfg), engine.WithSuspensionTTL(0))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restarted.Stop(context.Background())) })
	require.NoError(t, restarted.Start(f.ctx))
	require.Eventually(t, func() bool { return restartedStore.attempts.Load() > 0 }, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, restarted.Stop(context.Background()))
	require.EqualValues(t, 1, resolutions.Load())
	status, configured := restarted.TaskSettlementStatus()
	require.True(t, configured)
	require.Zero(t, status.Failures)
}
