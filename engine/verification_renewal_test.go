package engine_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/verification"
)

type renewingAdapter struct {
	*workerAdapter
	entered       chan struct{}
	release       chan struct{}
	lateReply     bool
	reconcileOnly bool
}

func (a *renewingAdapter) Evaluate(ctx context.Context, d verification.EvaluationDispatch) (verification.CallResult, error) {
	if a.reconcileOnly {
		return a.workerAdapter.Evaluate(ctx, d)
	}
	close(a.entered)
	select {
	case <-ctx.Done():
		if a.lateReply {
			return a.workerAdapter.Evaluate(ctx, d)
		}
		return verification.CallResult{}, ctx.Err()
	case <-a.release:
		return a.workerAdapter.Evaluate(ctx, d)
	}
}

type renewalStore struct {
	*sqlite.Store
	renew func(context.Context, verification.JobLease, time.Duration) (verification.Job, error)
}

func (s renewalStore) RenewVerificationJob(ctx context.Context, l verification.JobLease, d time.Duration) (verification.Job, error) {
	return s.renew(ctx, l, d)
}

func renewalPolicy() *verification.LeaseRenewalPolicy {
	return &verification.LeaseRenewalPolicy{Duration: time.Second, Interval: 100 * time.Millisecond}
}

func TestVerificationLeaseRenewalKeepsLongEvaluationOwned(t *testing.T) {
	f := newWorkerFixture(t)
	f.request.Claim.Duration = time.Second
	f.config.LeaseRenewal = renewalPolicy()
	adapter := &renewingAdapter{workerAdapter: f.adapter, entered: make(chan struct{}), release: make(chan struct{})}
	f.config.Adapter = adapter
	var renewals atomic.Int32
	wrapped := renewalStore{Store: f.storage, renew: func(ctx context.Context, l verification.JobLease, d time.Duration) (verification.Job, error) {
		renewals.Add(1)
		return f.storage.RenewVerificationJob(ctx, l, d)
	}}
	e := f.engine(t, wrapped)
	f.config.LeaseRenewal.Duration = 0 // The configured policy must be detached.
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := e.VerifyRun(ctx, f.request); done <- err }()
	select {
	case <-adapter.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("evaluation did not start")
	}
	before, err := f.storage.GetVerificationJob(f.ctx, f.request.Job.ID)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return time.Now().After(before.Lease.ExpiresAt) }, 3*time.Second, 10*time.Millisecond)
	_, err = f.storage.ClaimVerificationJob(f.ctx, verification.JobClaim{JobID: before.Request.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "other", Duration: time.Second})
	require.ErrorIs(t, err, verification.ErrJobConflict)
	close(adapter.release)
	require.NoError(t, <-done)
	require.GreaterOrEqual(t, renewals.Load(), int32(3))
	saved, err := f.storage.GetVerificationJob(f.ctx, f.request.Job.ID)
	require.NoError(t, err)
	require.Equal(t, verification.JobCommitted, saved.State)
	require.Equal(t, before.Generation, saved.Generation)
	require.Equal(t, 1, f.adapter.evaluations)
	stopped := renewals.Load()
	require.Never(t, func() bool { return renewals.Load() != stopped }, 250*time.Millisecond, 10*time.Millisecond)
}

func TestVerificationLeaseRenewalFailureCancelsPendingEvaluation(t *testing.T) {
	for _, mode := range []string{"failure", "lost acknowledgement", "late acknowledgement", "revoked", "blocked", "invalid owner", "regressed expiry", "deadline", "late reply", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.request.Claim.Duration = time.Second
			f.config.LeaseRenewal = renewalPolicy()
			if mode == "deadline" {
				f.request.Job.Deadline = time.Now().Add(time.Second)
			}
			adapter := &renewingAdapter{workerAdapter: f.adapter, entered: make(chan struct{}), release: make(chan struct{}), lateReply: mode == "late reply"}
			f.config.Adapter = adapter
			var revoked atomic.Bool
			f.config.Authorizer = workerAuthorizer(func(context.Context, verification.Binding) error {
				if revoked.Load() {
					return errors.New("revoked")
				}
				return nil
			})
			wrapped := renewalStore{Store: f.storage, renew: func(ctx context.Context, l verification.JobLease, d time.Duration) (verification.Job, error) {
				select {
				case <-adapter.entered:
					switch mode {
					case "failure", "late reply":
						return verification.Job{}, errors.New("renewal unavailable")
					case "lost acknowledgement":
						_, err := f.storage.RenewVerificationJob(ctx, l, d)
						if err != nil {
							return verification.Job{}, err
						}
						return verification.Job{}, errors.New("lost renewal reply")
					case "blocked":
						<-ctx.Done()
						return verification.Job{}, ctx.Err()
					case "late acknowledgement":
						job, renewErr := f.storage.RenewVerificationJob(ctx, l, d)
						if renewErr != nil {
							return verification.Job{}, renewErr
						}
						<-ctx.Done()
						return job, nil
					case "invalid owner", "regressed expiry":
						job, readErr := f.storage.GetVerificationJob(ctx, l.JobID)
						if readErr != nil {
							return verification.Job{}, readErr
						}
						if mode == "invalid owner" {
							job.Lease.Owner = "different"
						} else {
							job.Lease.ExpiresAt = job.Lease.ExpiresAt.Add(-time.Millisecond)
						}
						return verification.SealJob(job)
					}
				default:
				}
				return f.storage.RenewVerificationJob(ctx, l, d)
			}}
			e := f.engine(t, wrapped)
			ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := e.VerifyRun(ctx, f.request); done <- err }()
			select {
			case <-adapter.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("evaluation did not start")
			}
			if mode == "revoked" {
				revoked.Store(true)
			}
			if mode == "cancelled" {
				cancel()
			}
			select {
			case err := <-done:
				require.Error(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("renewal did not cancel evaluation")
			}
			saved, err := f.storage.GetVerificationJob(f.ctx, f.request.Job.ID)
			require.NoError(t, err)
			require.Len(t, saved.Calls, 1)
			require.Nil(t, saved.Calls[0].Completion)
			require.Equal(t, verification.JobRunning, saved.State)
			if mode == "late reply" {
				require.Equal(t, 1, f.adapter.evaluations)
			} else {
				require.Zero(t, f.adapter.evaluations)
			}
		})
	}
}

func TestVerificationLeaseRenewalConfigurationIsValidated(t *testing.T) {
	for _, policy := range []verification.LeaseRenewalPolicy{{}, {Duration: time.Second}, {Duration: time.Second, Interval: time.Second}, {Duration: verification.MaxJobLease + time.Second, Interval: time.Second}} {
		f := newWorkerFixture(t)
		f.config.LeaseRenewal = &policy
		_, err := engine.New(engine.WithStore(f.storage), engine.WithVerificationWorker(f.config))
		require.Error(t, err)
	}
}

func (a *renewingAdapter) Reconcile(ctx context.Context, d verification.EvaluationDispatch) (verification.CallResult, error) {
	if !a.reconcileOnly {
		return a.workerAdapter.Reconcile(ctx, d)
	}
	close(a.entered)
	select {
	case <-ctx.Done():
		return verification.CallResult{}, ctx.Err()
	case <-a.release:
		return a.workerAdapter.Reconcile(ctx, d)
	}
}

func TestVerificationLeaseRenewalProtectsReconciliation(t *testing.T) {
	f := newWorkerFixture(t)
	f.request.Claim.Duration = time.Second
	f.adapter.lost = true
	_, err := f.engine(t, f.storage).VerifyRun(f.ctx, f.request)
	require.Error(t, err)
	before, err := f.storage.GetVerificationJob(f.ctx, f.request.Job.ID)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return time.Now().After(before.Lease.ExpiresAt) }, 3*time.Second, 10*time.Millisecond)
	f.request.Claim.ClaimID = id.New(verification.PrefixJobClaim)
	f.config.LeaseRenewal = renewalPolicy()
	adapter := &renewingAdapter{workerAdapter: f.adapter, entered: make(chan struct{}), release: make(chan struct{}), reconcileOnly: true}
	f.config.Adapter = adapter
	e := f.engine(t, f.storage)
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, verifyErr := e.VerifyRun(ctx, f.request); done <- verifyErr }()
	select {
	case <-adapter.entered:
	case <-ctx.Done():
		t.Fatal("reconciliation did not start")
	}
	claimed, err := f.storage.GetVerificationJob(f.ctx, f.request.Job.ID)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return time.Now().After(claimed.Lease.ExpiresAt) }, 3*time.Second, 10*time.Millisecond)
	close(adapter.release)
	require.NoError(t, <-done)
	require.Equal(t, 1, f.adapter.evaluations)
	require.Equal(t, 1, f.adapter.reconciliations)
	after, err := f.storage.GetVerificationJob(f.ctx, f.request.Job.ID)
	require.NoError(t, err)
	require.Equal(t, verification.JobCommitted, after.State)
	require.Equal(t, before.Calls[0].Request.ID, after.Calls[0].Request.ID)
	require.Equal(t, before.Generation+1, after.Generation)
}
