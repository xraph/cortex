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

type orchestrationSource func(context.Context, coordination.OrchestrationListRequest) (coordination.OrchestrationPage, error)

func (f orchestrationSource) ListOrchestrations(ctx context.Context, r coordination.OrchestrationListRequest) (coordination.OrchestrationPage, error) {
	return f(ctx, r)
}

type orchestrationExpiry func(context.Context, id.OrchestrationID) error

func (f orchestrationExpiry) Expire(ctx context.Context, key id.OrchestrationID) error {
	return f(ctx, key)
}

func TestOrchestrationExpiryServiceRetainsIdentityAndDrains(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "team-expiry"}}}
		key := id.NewOrchestrationID()
		type identityKey struct{}
		startup, cancel := context.WithCancel(context.WithValue(context.Background(), identityKey{}, "host-expiry"))
		defer cancel()
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		service, err := coordination.NewOrchestrationExpiryService(coordination.OrchestrationExpiryConfig{
			Discovery: orchestrationSource(func(_ context.Context, r coordination.OrchestrationListRequest) (coordination.OrchestrationPage, error) {
				require.Positive(t, r.Limit)
				return coordination.OrchestrationPage{Teams: []coordination.ScheduledOrchestration{{Scope: scope, ID: key}}}, nil
			}),
			Expirer: orchestrationExpiry(func(ctx context.Context, team id.OrchestrationID) error {
				require.Equal(t, "host-expiry", ctx.Value(identityKey{}))
				require.Equal(t, scope, cortex.ScopeFromContext(ctx))
				require.Equal(t, key, team)
				calls.Add(1)
				close(entered)
				<-release
				return ctx.Err()
			}), MaxInFlight: 1,
		})
		require.NoError(t, err)
		require.NoError(t, service.Start(startup))
		<-entered
		cancel()
		synctest.Wait()
		require.True(t, service.Status().Running)
		stop, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		require.ErrorIs(t, service.Stop(stop), context.DeadlineExceeded)
		require.True(t, service.Status().Stopping)
		require.Equal(t, int32(1), calls.Load())
		close(release)
		require.NoError(t, service.Stop(context.Background()))
		require.Zero(t, service.Status().Active)
	})
}

func TestOrchestrationExpiryServiceRejectsWrongIdentityAndBoundsAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "expiry-bounds"}}}
		var calls, discoveries atomic.Int32
		source := orchestrationSource(func(context.Context, coordination.OrchestrationListRequest) (coordination.OrchestrationPage, error) {
			return coordination.OrchestrationPage{Teams: []coordination.ScheduledOrchestration{{Scope: scope, ID: id.NewAgentRunID()}}}, nil
		})
		config := coordination.OrchestrationExpiryConfig{Discovery: source, Expirer: orchestrationExpiry(func(context.Context, id.OrchestrationID) error { calls.Add(1); return nil })}
		service, err := coordination.NewOrchestrationExpiryService(config)
		require.NoError(t, err)
		require.NoError(t, service.Start(context.Background()))
		synctest.Wait()
		require.Zero(t, calls.Load())
		require.EqualValues(t, 1, service.Status().Failures)
		require.NoError(t, service.Stop(context.Background()))
		config.Discovery = orchestrationSource(func(context.Context, coordination.OrchestrationListRequest) (coordination.OrchestrationPage, error) {
			if discoveries.Add(1) > 1 {
				return coordination.OrchestrationPage{}, nil
			}
			return coordination.OrchestrationPage{Teams: []coordination.ScheduledOrchestration{{Scope: scope, ID: id.NewOrchestrationID()}}}, nil
		})
		config.AttemptTimeout = time.Second
		config.Expirer = orchestrationExpiry(func(ctx context.Context, _ id.OrchestrationID) error { calls.Add(1); <-ctx.Done(); return ctx.Err() })
		service, err = coordination.NewOrchestrationExpiryService(config)
		require.NoError(t, err)
		require.NoError(t, service.Start(context.Background()))
		synctest.Wait()
		require.Equal(t, 1, service.Status().Active)
		time.Sleep(time.Second)
		synctest.Wait()
		require.Zero(t, service.Status().Active)
		require.EqualValues(t, 1, service.Status().Failures)
		require.NoError(t, service.Stop(context.Background()))
	})
}
