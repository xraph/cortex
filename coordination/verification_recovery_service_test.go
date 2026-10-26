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
	"github.com/xraph/cortex/verification"
)

type verificationJobSource func(context.Context, coordination.VerificationJobListRequest) (coordination.VerificationJobPage, error)

func (f verificationJobSource) ListCapturedVerificationJobs(ctx context.Context, r coordination.VerificationJobListRequest) (coordination.VerificationJobPage, error) {
	return f(ctx, r)
}

type verificationRecoverer func(context.Context, id.ID) error

func (f verificationRecoverer) Recover(ctx context.Context, key id.ID) error { return f(ctx, key) }

func TestVerificationRecoveryServiceDrainsRestartsAndRejectsMalformedDiscovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "verification"}}}
		key := id.New(verification.PrefixJob)
		entered, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		config := coordination.VerificationRecoveryConfig{Discovery: verificationJobSource(func(context.Context, coordination.VerificationJobListRequest) (coordination.VerificationJobPage, error) {
			return coordination.VerificationJobPage{Jobs: []coordination.ScheduledVerificationJob{{Scope: scope, ID: key}}}, nil
		}), Recoverer: verificationRecoverer(func(ctx context.Context, got id.ID) error {
			require.Equal(t, key, got)
			require.Equal(t, scope, cortex.ScopeFromContext(ctx))
			if calls.Add(1) == 1 {
				close(entered)
				<-ctx.Done()
				<-release
				return ctx.Err()
			}
			return nil
		}), MaxInFlight: 1, AttemptTimeout: time.Second}
		service, err := coordination.NewVerificationRecoveryService(config)
		require.NoError(t, err)
		require.NoError(t, service.Start(context.Background()))
		<-entered
		stop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		require.ErrorIs(t, service.Stop(stop), context.DeadlineExceeded)
		require.True(t, service.Status().Stopping)
		require.ErrorIs(t, service.Start(context.Background()), coordination.ErrSchedulerStopped)
		close(release)
		require.NoError(t, service.Stop(context.Background()))
		require.Zero(t, service.Status().Active)
		require.NoError(t, service.Start(context.Background()))
		synctest.Wait()
		require.GreaterOrEqual(t, calls.Load(), int32(2))
		require.NoError(t, service.Stop(context.Background()))
		before := calls.Load()
		for _, page := range []coordination.VerificationJobPage{
			{Jobs: []coordination.ScheduledVerificationJob{{Scope: scope, ID: id.NewAgentRunID()}}},
			{Jobs: []coordination.ScheduledVerificationJob{{Scope: scope, ID: key}, {Scope: scope, ID: key}}},
			{Jobs: []coordination.ScheduledVerificationJob{{ID: key}}},
		} {
			config.Discovery = verificationJobSource(func(context.Context, coordination.VerificationJobListRequest) (coordination.VerificationJobPage, error) {
				return page, nil
			})
			service, err = coordination.NewVerificationRecoveryService(config)
			require.NoError(t, err)
			require.NoError(t, service.Start(context.Background()))
			synctest.Wait()
			require.EqualValues(t, 1, service.Status().Failures)
			require.Equal(t, before, calls.Load())
			require.NoError(t, service.Stop(context.Background()))
		}
	})
}

func TestVerificationRecoveryServiceSeparatesScopes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		scopes := []cortex.Scope{{Levels: []cortex.Level{{Key: "workspace", Value: "a"}}}, {Levels: []cortex.Level{{Key: "workspace", Value: "b"}}}}
		started := make(chan string, 3)
		jobs := []coordination.ScheduledVerificationJob{{Scope: scopes[0], ID: id.New(verification.PrefixJob)}, {Scope: scopes[0], ID: id.New(verification.PrefixJob)}, {Scope: scopes[1], ID: id.New(verification.PrefixJob)}}
		service, err := coordination.NewVerificationRecoveryService(coordination.VerificationRecoveryConfig{Discovery: verificationJobSource(func(context.Context, coordination.VerificationJobListRequest) (coordination.VerificationJobPage, error) {
			return coordination.VerificationJobPage{Jobs: jobs}, nil
		}), Recoverer: verificationRecoverer(func(ctx context.Context, _ id.ID) error {
			started <- cortex.ScopeFromContext(ctx).Canonical()
			<-ctx.Done()
			return ctx.Err()
		}), MaxInFlight: 3, MaxPerScope: 1})
		require.NoError(t, err)
		require.NoError(t, service.Start(context.Background()))
		t.Cleanup(func() { require.NoError(t, service.Stop(context.Background())) })
		synctest.Wait()
		require.Len(t, started, 2)
		first, second := <-started, <-started
		require.NotEqual(t, first, second)
		require.NoError(t, service.Stop(context.Background()))
	})
}
