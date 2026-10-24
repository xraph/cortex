package engine_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
)

type recoveryLifecycleClock struct{}

func (recoveryLifecycleClock) Now() time.Time { return time.Now() }

type recoveryShutdownHook struct{ calls atomic.Int32 }

func (*recoveryShutdownHook) Name() string                       { return "recovery-shutdown" }
func (h *recoveryShutdownHook) OnShutdown(context.Context) error { h.calls.Add(1); return nil }

func TestEngineClaimRecoveryExpiresStoredReservation(t *testing.T) {
	f := newTaskClaimRuntimeFixture(t, false)
	request := f.request
	request.Acquisition.Claim.Duration = time.Second
	receipt, err := f.f.storage.ClaimReadyTask(f.f.ctx, request)
	require.NoError(t, err)
	claim := receipt.Acquisition.Claim
	cfg := coordination.ClaimRecoveryConfig{Store: f.f.storage, Clock: recoveryLifecycleClock{}, Interval: 10 * time.Millisecond, Discovery: lifecyclePlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
		return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: claim.Scope, PlanID: claim.PlanID}}}, nil
	})}
	require.NoError(t, engine.WithClaimRecovery(cfg)(f.engine))
	require.NoError(t, f.engine.Start(f.f.ctx))
	t.Cleanup(func() { require.NoError(t, f.engine.Stop(context.Background())) })
	require.Eventually(t, func() bool {
		current, readErr := f.f.storage.GetTaskClaim(f.f.ctx, claim.PlanID, claim.TaskID)
		return readErr == nil && current.State == coordination.ClaimReleased
	}, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, f.engine.Stop(context.Background()))
	status, configured := f.engine.ClaimRecoveryStatus()
	require.True(t, configured)
	require.False(t, status.Running)
	require.Zero(t, status.Active)
	require.Equal(t, 0, f.client.calls)
	budget, err := f.f.storage.GetBudgetSnapshot(f.f.ctx, claim.PlanID)
	require.NoError(t, err)
	require.Equal(t, request.Acquisition.Reservation.Allocation, budget.Charged)
	require.NoError(t, f.engine.Start(f.f.ctx))
	require.NoError(t, f.engine.Stop(context.Background()))
	current, err := f.f.storage.GetTaskClaim(f.f.ctx, claim.PlanID, claim.TaskID)
	require.NoError(t, err)
	require.Equal(t, claim.Revision+1, current.Revision)
}

func TestEngineClaimRecoveryCancelsAllServicesBeforeDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		executionEntered, recoveryEntered, recoveryCancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		scheduler := engine.TaskSchedulingConfig{Worker: engine.TaskWorkerConfig{Resolver: struct{ engine.TaskWorkResolver }{}, Dispatch: struct {
			coordination.TaskDispatchAdmitter
		}{}}, Service: coordination.TaskServiceConfig{Scheduler: coordination.SchedulerConfig{Source: struct{ coordination.SchedulingSource }{}, Resolver: struct{ coordination.TaskClaimResolver }{}, Admitter: struct{ coordination.TaskClaimAdmitter }{}, Clock: recoveryLifecycleClock{}}, Discovery: lifecyclePlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			close(executionEntered)
			<-release
			return coordination.TaskPlanPage{}, nil
		})}}
		recovery := coordination.ClaimRecoveryConfig{Store: struct{ coordination.ClaimExpiryStore }{}, Clock: recoveryLifecycleClock{}, Discovery: lifecyclePlanSource(func(ctx context.Context, _ coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			close(recoveryEntered)
			<-ctx.Done()
			close(recoveryCancelled)
			return coordination.TaskPlanPage{}, ctx.Err()
		})}
		providerEntered, providerCancelled := make(chan struct{}), make(chan struct{})
		provider := lifecycleProviderConfig(lifecyclePlanSource(func(ctx context.Context, _ coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			close(providerEntered)
			<-ctx.Done()
			close(providerCancelled)
			return coordination.TaskPlanPage{}, ctx.Err()
		}))
		effectEntered, effectCancelled := make(chan struct{}), make(chan struct{})
		effect := lifecycleEffectConfig(lifecyclePlanSource(func(ctx context.Context, _ coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			close(effectEntered)
			<-ctx.Done()
			close(effectCancelled)
			return coordination.TaskPlanPage{}, ctx.Err()
		}))
		hook := new(recoveryShutdownHook)
		e, err := engine.New(engine.WithTaskScheduling(scheduler), engine.WithClaimRecovery(recovery), engine.WithProviderRecovery(provider), engine.WithEffectRecovery(effect), engine.WithTaskRunAdmitter(struct{ coordination.TaskRunAdmitter }{}), engine.WithTaskCallGuard(struct{ engine.TaskCallGuard }{}), engine.WithExtension(hook), engine.WithSuspensionTTL(0))
		require.NoError(t, err)
		require.NoError(t, e.Start(context.Background()))
		<-executionEntered
		<-recoveryEntered
		<-providerEntered
		<-effectEntered
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.ErrorIs(t, e.Stop(stop), context.DeadlineExceeded)
		select {
		case <-recoveryCancelled:
		default:
			t.Fatal("recovery discovery survived another service's drain timeout")
		}
		select {
		case <-providerCancelled:
		default:
			t.Fatal("provider discovery survived another service's drain timeout")
		}
		select {
		case <-effectCancelled:
		default:
			t.Fatal("effect recovery survived another service drain timeout")
		}
		require.Zero(t, hook.calls.Load())
		require.ErrorIs(t, e.Start(context.Background()), coordination.ErrSchedulerStopped)
		unblock()
		require.NoError(t, e.Stop(context.Background()))
		require.Equal(t, int32(1), hook.calls.Load())
		status, configured := e.ClaimRecoveryStatus()
		require.True(t, configured)
		require.False(t, status.Running)
	})
}

type recoveryBlockingShutdown struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (*recoveryBlockingShutdown) Name() string { return "recovery-blocking-shutdown" }
func (h *recoveryBlockingShutdown) OnShutdown(context.Context) error {
	if h.calls.Add(1) == 1 {
		close(h.entered)
		<-h.release
	}
	return nil
}

func TestEngineClaimRecoverySerializesStartupWithShutdownNotification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hook := &recoveryBlockingShutdown{entered: make(chan struct{}), release: make(chan struct{})}
		cfg := coordination.ClaimRecoveryConfig{Store: struct{ coordination.ClaimExpiryStore }{}, Clock: recoveryLifecycleClock{}, Discovery: lifecyclePlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			return coordination.TaskPlanPage{}, nil
		})}
		e, err := engine.New(engine.WithClaimRecovery(cfg), engine.WithExtension(hook), engine.WithSuspensionTTL(0))
		require.NoError(t, err)
		require.NoError(t, e.Start(context.Background()))
		stopped := make(chan error, 1)
		go func() { stopped <- e.Stop(context.Background()) }()
		<-hook.entered
		var once sync.Once
		finish := func() { once.Do(func() { close(hook.release); require.NoError(t, <-stopped) }) }
		defer func() { finish(); require.NoError(t, e.Stop(context.Background())) }()
		restart, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.ErrorIs(t, e.Start(restart), context.DeadlineExceeded, "startup must wait for the preceding shutdown notification")
		secondStop, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		require.ErrorIs(t, e.Stop(secondStop), context.DeadlineExceeded)
		require.Equal(t, int32(1), hook.calls.Load(), "queued Stop must not emit another shutdown notification")
		finish()
		require.NoError(t, e.Start(context.Background()))
	})
}
