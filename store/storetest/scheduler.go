package storetest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/store"
)

type schedulerStoreFixture struct {
	plans    planning.Store
	request  coordination.ReadyTaskClaimRequest
	calls    atomic.Int32
	distinct bool
}

func (f *schedulerStoreFixture) LoadSchedulingState(ctx context.Context, planID id.ID) (planning.Plan, []planning.TaskState, error) {
	p, err := f.plans.GetPlan(ctx, planID)
	// Deliberately stale states exercise the admission transaction's final checks.
	return p, nil, err
}
func (f *schedulerStoreFixture) ResolveTaskClaim(context.Context, planning.Plan, id.ID) (coordination.ReadyTaskClaimRequest, error) {
	req, _, err := f.request.Freeze()
	if err == nil && f.distinct {
		req.Acquisition.Claim.ID = id.New(coordination.PrefixClaim)
		req.Acquisition.Reservation.ID = id.New(execution.PrefixReservation)
		req.Acquisition.Reservation.OwnerRef = req.Acquisition.Claim.ID.String()
	}
	return req, err
}
func (f *schedulerStoreFixture) RunClaimedTask(context.Context, coordination.ReadyTaskClaimRequest, coordination.ReadyTaskClaim) error {
	f.calls.Add(1)
	return nil
}
func (f *schedulerStoreFixture) Now() time.Time { return time.Now() }

func testSchedulerDurableAdmission(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, distinct := range []bool{false, true} {
		name := "same_request"
		if distinct {
			name = "competing_requests"
		}
		t.Run(name, func(t *testing.T) {
			backend := newStore(t)
			ctx, req := readyClaimFixture(t, backend)
			fixture := &schedulerStoreFixture{plans: resultCapability[planning.Store](t, backend), request: req, distinct: distinct}
			admitter, err := coordination.NewTaskClaimAdmitter(resultCapability[coordination.ReadyTaskClaimStore](t, backend), readyClaimFenceFixture(func(_ context.Context, req coordination.ReadyTaskClaimRequest, next func(coordination.ReadyTaskClaimRequest) error) error {
				return next(req)
			}), readyClaimAuthorizerFixture(func(context.Context, coordination.ReadyTaskClaimRequest) error { return nil }))
			require.NoError(t, err)
			cfg := coordination.SchedulerConfig{Source: fixture, Resolver: fixture, Admitter: admitter, Worker: fixture, Clock: fixture, MaxConcurrent: 4, MaxPerScope: 4, TickTimeout: time.Minute}
			schedulers := make([]*coordination.TaskScheduler, 2)
			for i := range schedulers {
				schedulers[i], err = coordination.NewScheduler(cfg)
				require.NoError(t, err)
			}
			start := make(chan struct{})
			results := make(chan error, 8)
			var wg sync.WaitGroup
			for i := range 8 {
				wg.Go(func() { <-start; results <- schedulers[i%2].Tick(ctx, req.Acquisition.Claim.PlanID) })
			}
			close(start)
			wg.Wait()
			close(results)
			successful := 0
			for result := range results {
				if result == nil {
					successful++
				} else {
					require.ErrorIs(t, result, coordination.ErrSchedulerReconciliationRequired)
				}
			}
			require.Equal(t, 1, successful)
			require.EqualValues(t, 1, fixture.calls.Load())
			for _, scheduler := range schedulers {
				require.NoError(t, scheduler.Shutdown(context.Background()))
			}
			budgets := resultCapability[execution.BudgetStore](t, backend)
			account, err := budgets.GetBudgetSnapshot(ctx, req.Acquisition.Claim.PlanID)
			require.NoError(t, err)
			require.Equal(t, 1, account.Reservations)
			require.Equal(t, req.Acquisition.Reservation.Allocation, account.Charged)
			restarted, err := coordination.NewScheduler(cfg)
			require.NoError(t, err)
			require.ErrorIs(t, restarted.Tick(ctx, req.Acquisition.Claim.PlanID), coordination.ErrSchedulerReconciliationRequired)
			require.EqualValues(t, 1, fixture.calls.Load())
			require.NoError(t, restarted.Shutdown(context.Background()))
		})
	}
	t.Run("grant_revoked", func(t *testing.T) {
		backend := newStore(t)
		ctx, req := readyClaimFixture(t, backend)
		fixture := &schedulerStoreFixture{plans: resultCapability[planning.Store](t, backend), request: req}
		admitter, err := coordination.NewTaskClaimAdmitter(resultCapability[coordination.ReadyTaskClaimStore](t, backend), readyClaimFenceFixture(func(_ context.Context, req coordination.ReadyTaskClaimRequest, next func(coordination.ReadyTaskClaimRequest) error) error {
			return next(req)
		}), readyClaimAuthorizerFixture(func(context.Context, coordination.ReadyTaskClaimRequest) error { return errors.New("grant revoked") }))
		require.NoError(t, err)
		scheduler, err := coordination.NewScheduler(coordination.SchedulerConfig{Source: fixture, Resolver: fixture, Admitter: admitter, Worker: fixture, Clock: fixture})
		require.NoError(t, err)
		require.ErrorIs(t, scheduler.Tick(ctx, req.Acquisition.Claim.PlanID), coordination.ErrSchedulerReconciliationRequired)
		require.Zero(t, fixture.calls.Load())
		require.NoError(t, scheduler.Shutdown(context.Background()))
		account, err := resultCapability[execution.BudgetStore](t, backend).GetBudgetSnapshot(ctx, req.Acquisition.Claim.PlanID)
		require.NoError(t, err)
		require.Zero(t, account.Reservations)
	})
}
