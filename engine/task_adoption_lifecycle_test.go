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

func lifecycleAdoptionConfig(source coordination.TaskPlanSource) coordination.TaskAdoptionServiceConfig {
	return coordination.TaskAdoptionServiceConfig{Discovery: source, Store: struct{ coordination.TaskAdoptionStore }{}, Resolver: struct {
		coordination.TaskRecoveryResolver
	}{}, Fence: struct{ coordination.TaskRecoveryFence }{}, Authorizer: struct {
		coordination.TaskRecoveryAuthorizer
	}{}, AuthorizeScan: func(context.Context, coordination.Claim, execution.RunCallSeal) error { return nil }}
}

func TestEngineTaskAdoptionOwnsShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		cfg := lifecycleAdoptionConfig(lifecyclePlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			close(entered)
			<-release
			return coordination.TaskPlanPage{}, nil
		}))
		hook := new(recoveryShutdownHook)
		e, err := engine.New(engine.WithTaskAdoption(cfg), engine.WithExtension(hook), engine.WithSuspensionTTL(0))
		require.NoError(t, err)
		require.ErrorIs(t, engine.WithTaskAdoption(cfg)(e), coordination.ErrInvalidScheduler)
		require.NoError(t, e.Start(context.Background()))
		<-entered
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.ErrorIs(t, e.Stop(stop), context.DeadlineExceeded)
		status, configured := e.TaskAdoptionStatus()
		require.True(t, configured)
		require.True(t, status.Stopping)
		require.Zero(t, hook.calls.Load())
		require.ErrorIs(t, e.Start(context.Background()), coordination.ErrSchedulerStopped)
		unblock()
		require.NoError(t, e.Stop(context.Background()))
		require.Equal(t, int32(1), hook.calls.Load())
		status, configured = e.TaskAdoptionStatus()
		require.True(t, configured)
		require.False(t, status.Running)
		require.Zero(t, status.Active)
	})
}
