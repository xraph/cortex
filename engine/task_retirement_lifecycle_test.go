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

func lifecycleRetirementConfig(source coordination.TaskPlanSource) coordination.TaskRetirementServiceConfig {
	return coordination.TaskRetirementServiceConfig{Discovery: source, Store: struct {
		coordination.TaskRetirementServiceStore
	}{}, Resolver: struct {
		coordination.TaskRetirementResolver
	}{}, Fence: struct {
		coordination.TaskRetirementFence
	}{}, Authorizer: struct {
		coordination.TaskRetirementAuthorizer
	}{}, AuthorizeScan: func(context.Context, coordination.Claim, execution.RunCallSeal) error { return nil }}
}

func TestEngineTaskRetirementOwnsShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		defer unblock()
		cfg := lifecycleRetirementConfig(lifecyclePlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			close(entered)
			<-release
			return coordination.TaskPlanPage{}, nil
		}))
		hook := new(recoveryShutdownHook)
		e, err := engine.New(engine.WithTaskRetirement(cfg), engine.WithExtension(hook), engine.WithSuspensionTTL(0))
		require.NoError(t, err)
		require.ErrorIs(t, engine.WithTaskRetirement(cfg)(e), coordination.ErrInvalidScheduler)
		require.NoError(t, e.Start(context.Background()))
		<-entered
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.ErrorIs(t, e.Stop(stop), context.DeadlineExceeded)
		status, configured := e.TaskRetirementStatus()
		require.True(t, configured)
		require.True(t, status.Stopping)
		require.Zero(t, hook.calls.Load())
		require.ErrorIs(t, e.Start(context.Background()), coordination.ErrSchedulerStopped)
		unblock()
		require.NoError(t, e.Stop(context.Background()))
		require.Equal(t, int32(1), hook.calls.Load())
		status, configured = e.TaskRetirementStatus()
		require.True(t, configured)
		require.False(t, status.Running)
		require.Zero(t, status.Active)
	})
}
