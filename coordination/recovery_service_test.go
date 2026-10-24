package coordination_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
)

type recoveryServiceStore struct {
	list   func(context.Context, coordination.ClaimListRequest) ([]coordination.Claim, error)
	expire func(context.Context, id.ID, id.ID, int64) (coordination.Claim, error)
}

func (s recoveryServiceStore) ListTaskClaims(ctx context.Context, r coordination.ClaimListRequest) ([]coordination.Claim, error) {
	return s.list(ctx, r)
}
func (s recoveryServiceStore) ExpireTaskClaim(ctx context.Context, p, t id.ID, r int64) (coordination.Claim, error) {
	return s.expire(ctx, p, t, r)
}

func TestClaimRecoveryServiceOwnsPagesAndIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, claim, _ := expiryFixture(t)
		type principalKey struct{}
		startup, cancel := context.WithCancel(context.WithValue(ctx, principalKey{}, "recovery-service"))
		defer cancel()
		var reads, writes, discoveries atomic.Int32
		var mu sync.Mutex
		retained := claim
		store := recoveryServiceStore{
			list: func(ctx context.Context, r coordination.ClaimListRequest) ([]coordination.Claim, error) {
				require.Equal(t, "recovery-service", ctx.Value(principalKey{}))
				require.Equal(t, claim.Scope, cortex.ScopeFromContext(ctx))
				require.Equal(t, 1, r.Limit)
				require.Equal(t, claim.PlanID, r.PlanID)
				reads.Add(1)
				if !r.AfterTaskID.IsNil() {
					require.Equal(t, claim.TaskID, r.AfterTaskID)
					return nil, nil
				}
				mu.Lock()
				defer mu.Unlock()
				return []coordination.Claim{retained}, nil
			},
			expire: func(_ context.Context, p, task id.ID, r int64) (coordination.Claim, error) {
				require.Equal(t, claim.PlanID, p)
				require.Equal(t, claim.TaskID, task)
				mu.Lock()
				defer mu.Unlock()
				next, err := coordination.ExpireClaim(retained, r, claim.ExpiresAt)
				retained = next
				writes.Add(1)
				return next, err
			},
		}
		cfg := coordination.ClaimRecoveryConfig{Store: store, Clock: schedulerClock{at: claim.ExpiresAt}, PageSize: 1, Interval: time.Second, Discovery: taskPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			if discoveries.Add(1) != 1 {
				return coordination.TaskPlanPage{}, nil
			}
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: claim.Scope, PlanID: claim.PlanID}}}, nil
		})}
		service, err := coordination.NewClaimRecoveryService(cfg)
		require.NoError(t, err)
		require.NoError(t, service.Start(startup))
		cancel()
		synctest.Wait()
		require.Equal(t, int32(1), reads.Load())
		require.Equal(t, int32(1), writes.Load())
		require.Equal(t, 1, service.Status().Active)
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal(t, int32(2), reads.Load())
		require.Zero(t, service.Status().Active)
		require.NoError(t, service.Stop(context.Background()))
		discoveries.Store(0)
		require.NoError(t, service.Start(context.WithValue(ctx, principalKey{}, "recovery-service")))
		synctest.Wait()
		require.Equal(t, int32(3), reads.Load())
		require.Equal(t, int32(1), writes.Load())
		service.RequestStop()
		require.NoError(t, service.Stop(context.Background()))
		require.False(t, service.Status().Running)
		require.Zero(t, service.Status().Failures)
	})
}

func TestClaimRecoveryServiceStopTimeoutRetainsOwnedWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, claim, _ := expiryFixture(t)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		var reads atomic.Int32
		store := recoveryServiceStore{list: func(context.Context, coordination.ClaimListRequest) ([]coordination.Claim, error) {
			close(entered)
			<-release
			return nil, nil
		}}
		service, err := coordination.NewClaimRecoveryService(coordination.ClaimRecoveryConfig{Store: store, Clock: schedulerClock{at: claim.ExpiresAt}, MaxInFlight: 1, Discovery: taskPlanSource(func(_ context.Context, r coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			reads.Add(1)
			require.Equal(t, 1, r.Limit)
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: claim.Scope, PlanID: claim.PlanID}}}, nil
		})})
		require.NoError(t, err)
		require.NoError(t, service.Start(ctx))
		<-entered
		stop, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		require.ErrorIs(t, service.Stop(stop), context.DeadlineExceeded)
		require.True(t, service.Status().Stopping)
		require.Equal(t, 1, service.Status().Active)
		require.ErrorIs(t, service.Start(ctx), coordination.ErrSchedulerStopped)
		require.Equal(t, int32(1), reads.Load())
		unblock()
		require.NoError(t, service.Stop(context.Background()))
		require.Zero(t, service.Status().Active)
	})
}

func TestClaimRecoveryServiceBoundsEachPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, claim, _ := expiryFixture(t)
		var discoveries atomic.Int32
		store := recoveryServiceStore{list: func(ctx context.Context, _ coordination.ClaimListRequest) ([]coordination.Claim, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		service, err := coordination.NewClaimRecoveryService(coordination.ClaimRecoveryConfig{Store: store, Clock: schedulerClock{at: claim.ExpiresAt}, PageTimeout: time.Second, Discovery: taskPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			if discoveries.Add(1) > 1 {
				return coordination.TaskPlanPage{}, nil
			}
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: claim.Scope, PlanID: claim.PlanID}}}, nil
		})})
		require.NoError(t, err)
		require.NoError(t, service.Start(ctx))
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		require.Equal(t, uint64(1), service.Status().Failures)
		require.Zero(t, service.Status().Active)
		require.NoError(t, service.Stop(ctx))
	})
}

func TestClaimRecoveryServiceRejectsUnboundedConfiguration(t *testing.T) {
	_, claim, _ := expiryFixture(t)
	base := coordination.ClaimRecoveryConfig{Store: recoveryServiceStore{}, Clock: schedulerClock{at: claim.ExpiresAt}, Discovery: taskPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
		return coordination.TaskPlanPage{}, nil
	})}
	for _, change := range []func(*coordination.ClaimRecoveryConfig){
		func(c *coordination.ClaimRecoveryConfig) { c.Store = nil }, func(c *coordination.ClaimRecoveryConfig) { c.Clock = nil }, func(c *coordination.ClaimRecoveryConfig) { c.Discovery = nil },
		func(c *coordination.ClaimRecoveryConfig) { c.PageSize = 257 }, func(c *coordination.ClaimRecoveryConfig) { c.PageSize = -1 },
		func(c *coordination.ClaimRecoveryConfig) { c.PageTimeout = time.Hour }, func(c *coordination.ClaimRecoveryConfig) { c.Interval = -1 },
		func(c *coordination.ClaimRecoveryConfig) { c.MaxInFlight = 4097 }, func(c *coordination.ClaimRecoveryConfig) { c.MaxPerScope = -1 },
	} {
		cfg := base
		change(&cfg)
		_, err := coordination.NewClaimRecoveryService(cfg)
		require.Error(t, err)
	}
}
