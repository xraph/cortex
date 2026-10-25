package coordination_test

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
)

type orchestrationRecovery func(context.Context, id.OrchestrationID) error

func (f orchestrationRecovery) Recover(ctx context.Context, key id.OrchestrationID) error {
	return f(ctx, key)
}

func TestOrchestrationRecoveryServiceBoundsAndDrains(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "recovery"}}}
		key := id.NewOrchestrationID()
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		service, err := coordination.NewOrchestrationRecoveryService(coordination.OrchestrationRecoveryConfig{
			Discovery: orchestrationSource(func(context.Context, coordination.OrchestrationListRequest) (coordination.OrchestrationPage, error) {
				return coordination.OrchestrationPage{Teams: []coordination.ScheduledOrchestration{{Scope: scope, ID: key}}}, nil
			}),
			Recoverer: orchestrationRecovery(func(ctx context.Context, team id.OrchestrationID) error {
				require.Equal(t, key, team)
				require.Equal(t, scope, cortex.ScopeFromContext(ctx))
				calls.Add(1)
				close(entered)
				<-ctx.Done()
				<-release
				return ctx.Err()
			}), MaxInFlight: 1, AttemptTimeout: time.Second,
		})
		require.NoError(t, err)
		require.NoError(t, service.Start(context.Background()))
		<-entered
		time.Sleep(time.Second)
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.ErrorIs(t, service.Stop(stop), context.DeadlineExceeded)
		require.True(t, service.Status().Stopping)
		require.EqualValues(t, 1, calls.Load())
		close(release)
		require.NoError(t, service.Stop(context.Background()))
		require.Zero(t, service.Status().Active)
	})
}

func TestOrchestrationRecoveryServiceRejectsAgentIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		config := coordination.OrchestrationRecoveryConfig{Discovery: orchestrationSource(func(context.Context, coordination.OrchestrationListRequest) (coordination.OrchestrationPage, error) {
			return coordination.OrchestrationPage{Teams: []coordination.ScheduledOrchestration{{Scope: cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "recovery"}}}, ID: id.NewAgentRunID()}}}, nil
		}), Recoverer: orchestrationRecovery(func(context.Context, id.OrchestrationID) error { calls.Add(1); return nil })}
		service, err := coordination.NewOrchestrationRecoveryService(config)
		require.NoError(t, err)
		require.NoError(t, service.Start(context.Background()))
		synctest.Wait()
		require.Zero(t, calls.Load())
		require.EqualValues(t, 1, service.Status().Failures)
		require.NoError(t, service.Stop(context.Background()))
		config.AttemptTimeout = 31 * time.Minute
		_, err = coordination.NewOrchestrationRecoveryService(config)
		require.ErrorIs(t, err, coordination.ErrInvalidScheduler)
	})
}
