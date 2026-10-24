package extension

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

type lifecycleDiscovery func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error)

func (f lifecycleDiscovery) ListScheduledPlans(ctx context.Context, r coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
	return f(ctx, r)
}

type lifecycleClock struct{}

func (lifecycleClock) Now() time.Time { return time.Now() }

type lifecycleShutdown struct{ calls atomic.Int32 }

func (*lifecycleShutdown) Name() string                       { return "lifecycle-shutdown" }
func (h *lifecycleShutdown) OnShutdown(context.Context) error { h.calls.Add(1); return nil }

func TestTaskLifecycleStopTimeoutPreservesStartedState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		releaseSource := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseSource()
		hook := new(lifecycleShutdown)
		// Discovery blocks before any task is offered. These embedded ports are
		// deliberately unreachable; this fixture qualifies lifecycle state only.
		cfg := engine.TaskSchedulingConfig{
			Worker: engine.TaskWorkerConfig{Resolver: struct{ engine.TaskWorkResolver }{}, Dispatch: struct {
				coordination.TaskDispatchAdmitter
			}{}},
			Service: coordination.TaskServiceConfig{
				Scheduler: coordination.SchedulerConfig{Source: struct{ coordination.SchedulingSource }{}, Resolver: struct{ coordination.TaskClaimResolver }{}, Admitter: struct{ coordination.TaskClaimAdmitter }{}, Clock: lifecycleClock{}},
				Discovery: lifecycleDiscovery(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
					close(entered)
					<-release
					return coordination.TaskPlanPage{}, nil
				}),
			},
		}
		eng, err := engine.New(engine.WithTaskScheduling(cfg), engine.WithTaskRunAdmitter(struct{ coordination.TaskRunAdmitter }{}), engine.WithTaskCallGuard(struct{ engine.TaskCallGuard }{}), engine.WithExtension(hook), engine.WithSuspensionTTL(0))
		require.NoError(t, err)
		extension := New()
		extension.eng = eng
		require.NoError(t, extension.Start(context.Background()))
		<-entered
		stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.ErrorIs(t, extension.Stop(stopCtx), context.DeadlineExceeded)
		require.True(t, extension.IsStarted())
		require.Zero(t, hook.calls.Load())
		require.ErrorIs(t, extension.Start(context.Background()), coordination.ErrSchedulerStopped)
		releaseSource()
		require.NoError(t, extension.Stop(context.Background()))
		require.False(t, extension.IsStarted())
		require.Equal(t, int32(1), hook.calls.Load())
	})
}
