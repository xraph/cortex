package engine_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
)

type providerLifecycleStorage interface {
	execution.RunCallSealStore
	execution.ProviderCallReceiptStore
	ListTaskClaims(context.Context, coordination.ClaimListRequest) ([]coordination.Claim, error)
}

func lifecycleProviderConfig(source coordination.TaskPlanSource) coordination.ProviderRecoveryServiceConfig {
	return coordination.ProviderRecoveryServiceConfig{Discovery: source, Recovery: execution.ProviderRecoveryConfig{
		Store: struct{ providerLifecycleStorage }{},
		Adapter: recoveryProviderProbe(func(context.Context, execution.RunCallRequest) (execution.ProviderCallReceipt, error) {
			panic("discovery offers no work")
		}),
		Fence:         recoveryProviderFence(func(context.Context, execution.RunCallRequest, func() error) error { panic("discovery offers no work") }),
		Authorize:     func(context.Context, execution.RunCallRequest) error { return nil },
		Authenticate:  func(context.Context, execution.RunCallRequest, execution.ProviderCallReceipt) error { return nil },
		AuthorizeScan: func(context.Context, execution.RunCallSeal) error { return nil },
	}}
}
func TestEngineProviderRecoveryOwnsShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		config := lifecycleProviderConfig(lifecyclePlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			close(entered)
			<-release
			return coordination.TaskPlanPage{}, nil
		}))
		hook := new(recoveryShutdownHook)
		e, err := engine.New(engine.WithProviderRecovery(config), engine.WithExtension(hook), engine.WithSuspensionTTL(0))
		require.NoError(t, err)
		require.ErrorIs(t, engine.WithProviderRecovery(config)(e), coordination.ErrInvalidScheduler)
		require.NoError(t, e.Start(context.Background()))
		<-entered
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.ErrorIs(t, e.Stop(stop), context.DeadlineExceeded)
		status, configured := e.ProviderRecoveryStatus()
		require.True(t, configured)
		require.True(t, status.Stopping)
		require.Zero(t, hook.calls.Load())
		require.ErrorIs(t, e.Start(context.Background()), coordination.ErrSchedulerStopped)
		unblock()
		require.NoError(t, e.Stop(context.Background()))
		require.Equal(t, int32(1), hook.calls.Load())
		status, configured = e.ProviderRecoveryStatus()
		require.True(t, configured)
		require.False(t, status.Running)
		require.Zero(t, status.Active)
	})
}
