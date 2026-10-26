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
	"github.com/xraph/cortex/id"
)

type completionSource func(context.Context, coordination.TaskCompletionListRequest) (coordination.TaskCompletionPage, error)

func (f completionSource) ListTaskCompletionAssignments(ctx context.Context, r coordination.TaskCompletionListRequest) (coordination.TaskCompletionPage, error) {
	return f(ctx, r)
}

func TestEngineCompletionAndReplanJoinDiscoveryBeforeShutdown(t *testing.T) {
	for _, name := range []string{"completion", "replanning"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				entered, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				defer unblock()
				block := func() { close(entered); <-release }
				var option engine.Option
				var status func(*engine.Engine) (coordination.TaskServiceStatus, bool)
				switch name {
				case "completion":
					cfg := coordination.TaskCompletionServiceConfig{
						Store: struct {
							coordination.TaskCompletionServiceStore
						}{},
						Discovery: completionSource(func(context.Context, coordination.TaskCompletionListRequest) (coordination.TaskCompletionPage, error) {
							block()
							return coordination.TaskCompletionPage{}, nil
						}),
						Resolver: struct {
							coordination.TaskCompletionResolver
						}{}, Authority: struct {
							coordination.TaskCompletionAuthority
						}{}, Reviewer: struct {
							coordination.TaskCompletionReviewer
						}{}, Acceptor: struct {
							coordination.TaskCompletionAcceptance
						}{},
					}
					option = engine.WithTaskCompletion(cfg)
					status = (*engine.Engine).TaskCompletionStatus
				case "replanning":
					cfg := coordination.PlanReplanServiceConfig{
						Store: struct{ coordination.PlanReplanStore }{},
						Discovery: lifecyclePlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
							block()
							return coordination.TaskPlanPage{}, nil
						}),
						AuthorizeScan: func(context.Context, id.ID) error { return nil }, Resolver: struct {
							coordination.PlanReplanResolver
						}{}, Fence: struct{ coordination.PlanReplanFence }{}, Authorizer: struct {
							coordination.PlanReplanAuthorizer
						}{}, Runtime: struct{ coordination.PlanReplanRuntime }{},
					}
					option = engine.WithPlanReplanning(cfg)
					status = (*engine.Engine).PlanReplanningStatus
				}
				hook := new(recoveryShutdownHook)
				e, err := engine.New(option, engine.WithExtension(hook), engine.WithSuspensionTTL(0))
				require.NoError(t, err)
				require.ErrorIs(t, option(e), coordination.ErrInvalidScheduler)
				require.NoError(t, e.Start(context.Background()))
				<-entered
				deadline, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				require.ErrorIs(t, e.Stop(deadline), context.DeadlineExceeded)
				state, configured := status(e)
				require.True(t, configured)
				require.True(t, state.Stopping)
				require.Zero(t, hook.calls.Load())
				require.ErrorIs(t, e.Start(context.Background()), coordination.ErrSchedulerStopped)
				unblock()
				require.NoError(t, e.Stop(context.Background()))
				require.Equal(t, int32(1), hook.calls.Load())
				state, configured = status(e)
				require.True(t, configured)
				require.False(t, state.Running)
				require.Zero(t, state.Active)
			})
		})
	}
}
