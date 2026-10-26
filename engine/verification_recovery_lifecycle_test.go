package engine_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func TestVerificationBackgroundRecoveryReopensAndReconciles(t *testing.T) {
	for _, mode := range []string{"queued", "lost_evaluation", "lost_commit"} {
		t.Run(mode, func(t *testing.T) {
			uncertain := mode == "lost_evaluation"
			f := newWorkerFixture(t)
			intent := f.registerIntent(t)
			commits := 0
			if mode == "lost_commit" {
				f.config.Fence = workerFence(func(_ context.Context, binding verification.Binding, commit func(verification.Binding) error) error {
					commits++
					if err := commit(binding); err != nil {
						return err
					}
					return errors.New("lost commit reply")
				})
			}
			var originalCall id.ID
			if uncertain {
				f.adapter.lost = true
				f.request.Claim.Duration = 250 * time.Millisecond
				_, err := f.engine(t, f.storage).VerifyRun(f.ctx, f.request)
				require.Error(t, err)
				job, err := f.storage.GetVerificationJob(f.ctx, f.request.Job.ID)
				require.NoError(t, err)
				require.Len(t, job.Calls, 1)
				originalCall = job.Calls[0].Request.ID
			} else {
				_, err := f.storage.EnqueueVerificationJob(f.ctx, f.request.Job)
				require.NoError(t, err)
			}
			generated, tools := f.llm.calls, f.llm.tools
			require.NoError(t, f.storage.Close())
			f.storage = openSQLiteStoreForTest(t, f.path)
			source, err := coordination.NewScopedVerificationSource(f.storage, []cortex.Scope{cortex.ScopeFromContext(f.ctx)}, func(context.Context, cortex.Scope) error { return nil })
			require.NoError(t, err)
			type hostIdentity struct{}
			var authorized atomic.Int32
			config := engine.VerificationRecoveryConfig{Owner: "background", LeaseDuration: time.Minute, Service: coordination.VerificationRecoveryConfig{Discovery: source, Interval: 5 * time.Millisecond}, Authorize: func(ctx context.Context, key id.ID) error {
				require.Equal(t, "host", ctx.Value(hostIdentity{}))
				require.Equal(t, f.request.Job.ID, key)
				authorized.Add(1)
				return nil
			}}
			fresh, err := engine.New(engine.WithVerificationRecovery(config), engine.WithVerificationWorker(f.config), engine.WithStore(f.storage), engine.WithLLM(f.llm))
			require.NoError(t, err)
			startup, cancel := context.WithCancel(context.WithValue(f.ctx, hostIdentity{}, "host"))
			require.NoError(t, fresh.Start(startup))
			cancel()
			t.Cleanup(func() { require.NoError(t, fresh.Stop(context.Background())) })
			require.Eventually(t, func() bool {
				job, readErr := f.storage.GetVerificationJob(f.ctx, f.request.Job.ID)
				return readErr == nil && job.State == verification.JobCommitted
			}, 10*time.Second, 5*time.Millisecond)
			require.NoError(t, fresh.Stop(context.Background()))
			require.Equal(t, generated, f.llm.calls)
			require.Equal(t, tools, f.llm.tools)
			require.Equal(t, 1, f.adapter.evaluations)
			if mode == "lost_commit" {
				require.Equal(t, 1, commits)
			}
			job, err := f.storage.GetVerificationJob(f.ctx, f.request.Job.ID)
			require.NoError(t, err)
			require.Equal(t, intent.ResultID, job.ResultID)
			if uncertain {
				require.Equal(t, 1, f.adapter.reconciliations)
				require.Equal(t, originalCall, job.Calls[0].Request.ID)
			}
			require.Positive(t, authorized.Load())
			status, configured := fresh.VerificationRecoveryStatus()
			require.True(t, configured)
			require.False(t, status.Running)
			require.Zero(t, status.Active)
			require.NoError(t, fresh.Start(context.WithValue(f.ctx, hostIdentity{}, "host")))
			require.NoError(t, fresh.Stop(context.Background()))
			require.Equal(t, 1, f.adapter.evaluations)
		})
	}
}

type blockingVerificationAdapter struct {
	verification.EvaluationAdapter
	entered, release chan struct{}
}

func (a *blockingVerificationAdapter) Evaluate(ctx context.Context, _ verification.EvaluationDispatch) (verification.CallResult, error) {
	close(a.entered)
	<-ctx.Done()
	<-a.release
	return verification.CallResult{}, ctx.Err()
}

func TestVerificationBackgroundRecoveryJoinsCancelledEvaluator(t *testing.T) {
	f := newWorkerFixture(t)
	f.registerIntent(t)
	_, err := f.storage.EnqueueVerificationJob(f.ctx, f.request.Job)
	require.NoError(t, err)
	blocked := &blockingVerificationAdapter{EvaluationAdapter: f.adapter, entered: make(chan struct{}), release: make(chan struct{})}
	f.config.Adapter = blocked
	source, err := coordination.NewScopedVerificationSource(f.storage, []cortex.Scope{cortex.ScopeFromContext(f.ctx)}, func(context.Context, cortex.Scope) error { return nil })
	require.NoError(t, err)
	config := engine.VerificationRecoveryConfig{Owner: "background", LeaseDuration: time.Minute, Service: coordination.VerificationRecoveryConfig{Discovery: source, Interval: 5 * time.Millisecond}, Authorize: func(context.Context, id.ID) error { return nil }}
	fresh, err := engine.New(engine.WithVerificationRecovery(config), engine.WithVerificationWorker(f.config), engine.WithStore(f.storage))
	require.NoError(t, err)
	var release sync.Once
	t.Cleanup(func() {
		release.Do(func() { close(blocked.release) })
		require.NoError(t, fresh.Stop(context.Background()))
	})
	require.NoError(t, fresh.Start(f.ctx))
	select {
	case <-blocked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("evaluator did not start")
	}
	stop, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, fresh.Stop(stop), context.DeadlineExceeded)
	status, configured := fresh.VerificationRecoveryStatus()
	require.True(t, configured)
	require.True(t, status.Stopping)
	require.Positive(t, status.Active)
	require.ErrorIs(t, fresh.Start(f.ctx), coordination.ErrSchedulerStopped)
	release.Do(func() { close(blocked.release) })
	require.NoError(t, fresh.Stop(context.Background()))
	job, err := f.storage.GetVerificationJob(f.ctx, f.request.Job.ID)
	require.NoError(t, err)
	require.Len(t, job.Calls, 1)
	require.Nil(t, job.Calls[0].Completion)
}

func TestVerificationBackgroundRecoveryRespectsLeaseAndCurrentAuthority(t *testing.T) {
	for _, mode := range []string{"lease", "job_denied", "binding_denied", "unmanaged"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			if mode != "unmanaged" {
				f.registerIntent(t)
			}
			_, err := f.storage.EnqueueVerificationJob(f.ctx, f.request.Job)
			require.NoError(t, err)
			if mode == "lease" {
				_, err = f.storage.ClaimVerificationJob(f.ctx, f.request.Claim)
				require.NoError(t, err)
			}
			if mode == "binding_denied" {
				f.config.Authorizer = workerAuthorizer(func(context.Context, verification.Binding) error { return errors.New("denied") })
			}
			source, err := coordination.NewScopedVerificationSource(f.storage, []cortex.Scope{cortex.ScopeFromContext(f.ctx)}, func(context.Context, cortex.Scope) error { return nil })
			require.NoError(t, err)
			var visited atomic.Int32
			config := engine.VerificationRecoveryConfig{Owner: "background", LeaseDuration: time.Minute, Service: coordination.VerificationRecoveryConfig{Discovery: source, Interval: 5 * time.Millisecond}, Authorize: func(context.Context, id.ID) error {
				visited.Add(1)
				if mode == "job_denied" {
					return errors.New("denied")
				}
				return nil
			}}
			fresh, err := engine.New(engine.WithVerificationRecovery(config), engine.WithVerificationWorker(f.config), engine.WithStore(f.storage))
			require.NoError(t, err)
			require.NoError(t, fresh.Start(f.ctx))
			t.Cleanup(func() { require.NoError(t, fresh.Stop(context.Background())) })
			require.Eventually(t, func() bool { return visited.Load() > 2 }, 5*time.Second, time.Millisecond)
			require.NoError(t, fresh.Stop(context.Background()))
			require.Zero(t, f.adapter.evaluations)
			require.Zero(t, f.adapter.estimates)
			if mode != "lease" {
				status, _ := fresh.VerificationRecoveryStatus()
				require.Positive(t, status.Failures)
			}
		})
	}
}

func TestVerificationBackgroundRecoveryRequiresWorker(t *testing.T) {
	f := newWorkerFixture(t)
	source, err := coordination.NewScopedVerificationSource(f.storage, []cortex.Scope{cortex.ScopeFromContext(f.ctx)}, func(context.Context, cortex.Scope) error { return nil })
	require.NoError(t, err)
	config := engine.VerificationRecoveryConfig{Owner: "background", LeaseDuration: time.Minute, Service: coordination.VerificationRecoveryConfig{Discovery: source}, Authorize: func(context.Context, id.ID) error { return nil }}
	_, err = engine.New(engine.WithStore(f.storage), engine.WithVerificationRecovery(config))
	require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
	config.Authorize = nil
	_, err = engine.New(engine.WithStore(f.storage), engine.WithVerificationRecovery(config))
	require.ErrorIs(t, err, coordination.ErrInvalidScheduler)
}
