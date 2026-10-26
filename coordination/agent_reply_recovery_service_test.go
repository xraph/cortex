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

type agentReplySource func(context.Context, coordination.AgentReplyListRequest) (coordination.AgentReplyPage, error)

func (f agentReplySource) ListAgentReplyRuns(ctx context.Context, req coordination.AgentReplyListRequest) (coordination.AgentReplyPage, error) {
	return f(ctx, req)
}

type agentReplyRecoverer func(context.Context, id.AgentRunID) error

func (f agentReplyRecoverer) Recover(ctx context.Context, key id.AgentRunID) error {
	return f(ctx, key)
}

func TestAgentReplyRecoveryServiceDrainsAndRejectsMalformedDiscovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "reply-recovery"}}}
		key := id.NewAgentRunID()
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		config := coordination.AgentReplyRecoveryConfig{Discovery: agentReplySource(func(context.Context, coordination.AgentReplyListRequest) (coordination.AgentReplyPage, error) {
			return coordination.AgentReplyPage{Runs: []coordination.ScheduledAgentReply{{Scope: scope, ID: key}}}, nil
		}), Recoverer: agentReplyRecoverer(func(ctx context.Context, runID id.AgentRunID) error {
			require.Equal(t, key, runID)
			require.Equal(t, scope, cortex.ScopeFromContext(ctx))
			calls.Add(1)
			close(entered)
			<-ctx.Done()
			<-release
			return ctx.Err()
		}), MaxInFlight: 1, AttemptTimeout: time.Second}
		service, err := coordination.NewAgentReplyRecoveryService(config)
		require.NoError(t, err)
		require.NoError(t, service.Start(context.Background()))
		<-entered
		stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		require.ErrorIs(t, service.Stop(stop), context.DeadlineExceeded)
		require.True(t, service.Status().Stopping)
		require.ErrorIs(t, service.Start(context.Background()), coordination.ErrSchedulerStopped)
		require.EqualValues(t, 1, calls.Load())
		close(release)
		require.NoError(t, service.Stop(context.Background()))
		require.Zero(t, service.Status().Active)
		config.Recoverer = agentReplyRecoverer(func(context.Context, id.AgentRunID) error { calls.Add(1); return nil })
		for _, malformed := range []coordination.AgentReplyPage{
			{Runs: []coordination.ScheduledAgentReply{{Scope: scope, ID: id.NewOrchestrationID()}}},
			{Runs: []coordination.ScheduledAgentReply{{Scope: scope, ID: key}, {Scope: scope, ID: key}}},
			{Runs: []coordination.ScheduledAgentReply{{ID: key}}},
		} {
			config.Discovery = agentReplySource(func(context.Context, coordination.AgentReplyListRequest) (coordination.AgentReplyPage, error) {
				return malformed, nil
			})
			service, err = coordination.NewAgentReplyRecoveryService(config)
			require.NoError(t, err)
			require.NoError(t, service.Start(context.Background()))
			synctest.Wait()
			require.EqualValues(t, 1, service.Status().Failures)
			require.EqualValues(t, 1, calls.Load())
			require.NoError(t, service.Stop(context.Background()))
		}
		config.AttemptTimeout = 31 * time.Minute
		_, err = coordination.NewAgentReplyRecoveryService(config)
		require.ErrorIs(t, err, coordination.ErrInvalidScheduler)
	})
}
