package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/verification"
)

func recoveryRequest() engine.VerificationRecoveryRequest {
	return engine.VerificationRecoveryRequest{Owner: "recovery", LeaseDuration: time.Minute, Limit: 10}
}

func TestVerificationRecoveryCommitsQueuedJobAfterReopen(t *testing.T) {
	f := newWorkerFixture(t)
	intent := f.registerIntent(t)
	_, err := f.storage.EnqueueVerificationJob(f.ctx, f.request.Job)
	require.NoError(t, err)
	generated, tools := f.llm.calls, f.llm.tools
	require.NoError(t, f.storage.Close())
	f.storage = openSQLiteStoreForTest(t, f.path)
	page, err := f.engine(t, f.storage).RecoverVerificationPage(f.ctx, recoveryRequest())
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, engine.RecoveryCommitted, page.Items[0].Status)
	require.Equal(t, intent.ResultID, page.Items[0].ResultID)
	require.Equal(t, 1, f.adapter.evaluations)
	require.Equal(t, generated, f.llm.calls)
	require.Equal(t, tools, f.llm.tools)
	page, err = f.engine(t, f.storage).RecoverVerificationPage(f.ctx, recoveryRequest())
	require.NoError(t, err)
	require.Equal(t, engine.RecoveryTerminal, page.Items[0].Status)
	require.Equal(t, 1, f.adapter.evaluations)
}

func TestVerificationRecoveryRespectsLiveLeaseAndUnmanagedJobs(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmanaged", true: "leased"}[managed], func(t *testing.T) {
			f := newWorkerFixture(t)
			if managed {
				f.registerIntent(t)
			}
			before, err := f.storage.EnqueueVerificationJob(f.ctx, f.request.Job)
			require.NoError(t, err)
			want := engine.RecoveryUnmanaged
			if managed {
				before, err = f.storage.ClaimVerificationJob(f.ctx, f.request.Claim)
				require.NoError(t, err)
				want = engine.RecoveryBusy
			}
			page, err := f.engine(t, f.storage).RecoverVerificationPage(f.ctx, recoveryRequest())
			require.NoError(t, err)
			require.Equal(t, want, page.Items[0].Status)
			after, err := f.storage.GetVerificationJob(f.ctx, before.Request.ID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Zero(t, f.adapter.estimates)
		})
	}
}

func TestVerificationRecoveryRejectsAuthorityAndScope(t *testing.T) {
	f := newWorkerFixture(t)
	f.registerIntent(t)
	before, err := f.storage.EnqueueVerificationJob(f.ctx, f.request.Job)
	require.NoError(t, err)
	f.config.Authorizer = workerAuthorizer(func(context.Context, verification.Binding) error { return errors.New("SECRET revoked") })
	e := f.engine(t, f.storage)
	page, err := e.RecoverVerificationPage(f.ctx, recoveryRequest())
	require.NoError(t, err)
	require.Equal(t, engine.RecoveryUnavailable, page.Items[0].Status)
	data, encodeErr := json.Marshal(page)
	require.NoError(t, encodeErr)
	require.NotContains(t, string(data), "SECRET")
	after, err := f.storage.GetVerificationJob(f.ctx, before.Request.ID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Zero(t, f.adapter.evaluations)
	_, err = e.RecoverVerificationPage(context.Background(), recoveryRequest())
	require.ErrorIs(t, err, cortex.ErrNoScope)
	page, err = e.RecoverVerificationPage(overlayCtx("other"), recoveryRequest())
	require.NoError(t, err)
	require.Empty(t, page.Items)
}

func TestVerificationRecoveryLoopScansPastTerminalPage(t *testing.T) {
	f := newWorkerFixture(t)
	f.registerIntent(t)
	e := f.engine(t, f.storage)
	_, err := e.VerifyRun(f.ctx, f.request)
	require.NoError(t, err)
	f.request.Job.ID = id.New(verification.PrefixJob)
	f.request.ResultID = id.New(verification.PrefixResult)
	f.registerIntent(t)
	_, err = f.storage.EnqueueVerificationJob(f.ctx, f.request.Job)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	pages := 0
	request := recoveryRequest()
	request.Limit = 1
	err = e.RunVerificationRecovery(ctx, engine.VerificationRecoveryLoop{Request: request, Interval: time.Millisecond, Observe: func(page engine.VerificationRecoveryPage) {
		pages++
		for _, item := range page.Items {
			if item.Status == engine.RecoveryCommitted {
				cancel()
			}
		}
	}})
	require.ErrorIs(t, err, context.Canceled)
	require.GreaterOrEqual(t, pages, 2)
	require.Equal(t, 2, f.adapter.evaluations)
}

func TestVerificationRecoveryValidatesRequest(t *testing.T) {
	f := newWorkerFixture(t)
	e := f.engine(t, f.storage)
	for _, mutate := range []func(*engine.VerificationRecoveryRequest){
		func(r *engine.VerificationRecoveryRequest) { r.Owner = "" },
		func(r *engine.VerificationRecoveryRequest) { r.LeaseDuration = 0 },
		func(r *engine.VerificationRecoveryRequest) { r.LeaseDuration = verification.MaxJobLease + time.Second },
		func(r *engine.VerificationRecoveryRequest) { r.Limit = 0 },
		func(r *engine.VerificationRecoveryRequest) { r.Limit = 257 },
		func(r *engine.VerificationRecoveryRequest) { r.After = id.New(verification.PrefixResult) },
	} {
		request := recoveryRequest()
		mutate(&request)
		_, err := e.RecoverVerificationPage(f.ctx, request)
		require.ErrorIs(t, err, verification.ErrInvalid)
	}
}

type recoveryListStore struct {
	*sqlite.Store
	list func(context.Context, verification.JobListRequest) ([]verification.Job, error)
}

func (s recoveryListStore) ListVerificationJobs(ctx context.Context, request verification.JobListRequest) ([]verification.Job, error) {
	return s.list(ctx, request)
}

func TestVerificationRecoveryCancellationDuringDiscovery(t *testing.T) {
	f := newWorkerFixture(t)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	wrapped := recoveryListStore{Store: f.storage, list: func(context.Context, verification.JobListRequest) ([]verification.Job, error) {
		cancel()
		return nil, nil
	}}
	_, err := f.engine(t, wrapped).RecoverVerificationPage(ctx, recoveryRequest())
	require.ErrorIs(t, err, context.Canceled)
}

func TestVerificationRecoveryPartialPagePreservesCursor(t *testing.T) {
	f := newWorkerFixture(t)
	f.registerIntent(t)
	first, err := f.storage.EnqueueVerificationJob(f.ctx, f.request.Job)
	require.NoError(t, err)
	f.request.Job.ID = id.New(verification.PrefixJob)
	f.request.ResultID = id.New(verification.PrefixResult)
	f.registerIntent(t)
	_, err = f.storage.EnqueueVerificationJob(f.ctx, f.request.Job)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	f.config.Authorizer = workerAuthorizer(func(context.Context, verification.Binding) error { cancel(); return context.Canceled })
	page, err := f.engine(t, f.storage).RecoverVerificationPage(ctx, recoveryRequest())
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, page.Items, 1)
	require.Equal(t, first.Request.ID, page.NextAfter)
}

func TestVerificationRecoveryRejectsMalformedDiscoveryBeforeWork(t *testing.T) {
	for _, mode := range []string{"cross scope", "duplicate", "too many", "changed digest", "cursor", "storage failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			f.registerIntent(t)
			job, err := f.storage.EnqueueVerificationJob(f.ctx, f.request.Job)
			require.NoError(t, err)
			request := recoveryRequest()
			ctx := f.ctx
			items := []verification.Job{job}
			if mode == "cross scope" {
				ctx = overlayCtx("other")
			}
			if mode == "duplicate" {
				items = append(items, job)
			}
			if mode == "too many" {
				items = append(items, job)
				request.Limit = 1
			}
			if mode == "changed digest" {
				items[0].Digest = "bad"
			}
			if mode == "cursor" {
				request.After = job.Request.ID
			}
			wrapped := recoveryListStore{Store: f.storage, list: func(context.Context, verification.JobListRequest) ([]verification.Job, error) {
				if mode == "storage failure" {
					return nil, errors.New("SECRET storage")
				}
				return items, nil
			}}
			_, err = f.engine(t, wrapped).RecoverVerificationPage(ctx, request)
			require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
			require.NotContains(t, err.Error(), "SECRET")
			require.Zero(t, f.adapter.evaluations)
		})
	}
}

func TestVerificationRecoveryReconcilesUncertainCallsAfterLeaseExpiry(t *testing.T) {
	for _, mode := range []string{"recover", "uncertain", "exhausted"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			if mode == "exhausted" {
				f.request.Job.MaxAttempts = 1
			}
			f.registerIntent(t)
			f.request.Claim.Duration = 2 * time.Second
			f.adapter.lost = true
			_, err := f.engine(t, f.storage).VerifyRun(f.ctx, f.request)
			require.Error(t, err)
			saved, err := f.storage.GetVerificationJob(f.ctx, f.request.Job.ID)
			require.NoError(t, err)
			require.Len(t, saved.Calls, 1)
			require.Equal(t, 1, f.adapter.evaluations)
			callID := saved.Calls[0].Request.ID
			require.NoError(t, f.storage.Close())
			f.storage = openSQLiteStoreForTest(t, f.path)
			e := f.engine(t, f.storage)
			page, err := e.RecoverVerificationPage(f.ctx, recoveryRequest())
			require.NoError(t, err)
			require.Equal(t, engine.RecoveryBusy, page.Items[0].Status)
			require.Zero(t, f.adapter.reconciliations)
			require.Eventually(t, func() bool { return time.Now().After(saved.Lease.ExpiresAt) }, 5*time.Second, 10*time.Millisecond)
			if mode == "uncertain" {
				f.adapter.retained = verification.CallResult{}
			}
			page, err = e.RecoverVerificationPage(f.ctx, recoveryRequest())
			require.NoError(t, err)
			want := engine.RecoveryCommitted
			if mode == "uncertain" {
				want = engine.RecoveryUncertain
			}
			if mode == "exhausted" {
				want = engine.RecoveryExpired
			}
			require.Equal(t, want, page.Items[0].Status)
			require.Equal(t, 1, f.adapter.evaluations)
			require.Equal(t, 2, f.llm.calls)
			require.Equal(t, 1, f.llm.tools)
			after, err := f.storage.GetVerificationJob(f.ctx, f.request.Job.ID)
			require.NoError(t, err)
			require.Equal(t, callID, after.Calls[0].Request.ID)
			if mode == "exhausted" {
				require.Equal(t, verification.JobInconclusive, after.State)
				require.Zero(t, f.adapter.reconciliations)
			} else {
				require.Equal(t, 1, f.adapter.reconciliations)
			}
		})
	}
}

func TestVerificationRecoveryLoopValidationAndCancellation(t *testing.T) {
	f := newWorkerFixture(t)
	f.registerIntent(t)
	_, err := f.storage.EnqueueVerificationJob(f.ctx, f.request.Job)
	require.NoError(t, err)
	e := f.engine(t, f.storage)
	for _, interval := range []time.Duration{0, -time.Second, time.Hour + time.Second} {
		require.ErrorIs(t, e.RunVerificationRecovery(f.ctx, engine.VerificationRecoveryLoop{Request: recoveryRequest(), Interval: interval}), verification.ErrInvalid)
	}
	require.Zero(t, f.adapter.evaluations)
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	require.ErrorIs(t, e.RunVerificationRecovery(ctx, engine.VerificationRecoveryLoop{Request: recoveryRequest(), Interval: time.Hour}), context.Canceled)
	require.Zero(t, f.adapter.evaluations)
}

func TestVerificationRecoveryDetachesDiscoveredPage(t *testing.T) {
	f := newWorkerFixture(t)
	f.registerIntent(t)
	first, err := f.storage.EnqueueVerificationJob(f.ctx, f.request.Job)
	require.NoError(t, err)
	f.request.Job.ID = id.New(verification.PrefixJob)
	f.request.ResultID = id.New(verification.PrefixResult)
	f.registerIntent(t)
	second, err := f.storage.EnqueueVerificationJob(f.ctx, f.request.Job)
	require.NoError(t, err)
	items := []verification.Job{first, second}
	changed := false
	f.config.Authorizer = workerAuthorizer(func(context.Context, verification.Binding) error {
		if !changed {
			changed = true
			items[1].State = verification.JobCommitted
		}
		return nil
	})
	wrapped := recoveryListStore{Store: f.storage, list: func(context.Context, verification.JobListRequest) ([]verification.Job, error) { return items, nil }}
	page, err := f.engine(t, wrapped).RecoverVerificationPage(f.ctx, recoveryRequest())
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	for _, item := range page.Items {
		require.Equal(t, engine.RecoveryCommitted, item.Status)
	}
	require.Equal(t, 2, f.adapter.evaluations)
}

type recoveryClaimStore struct {
	*sqlite.Store
	lost bool
}

func (s *recoveryClaimStore) ClaimVerificationJob(ctx context.Context, claim verification.JobClaim) (verification.Job, error) {
	job, err := s.Store.ClaimVerificationJob(ctx, claim)
	if err == nil && s.lost {
		s.lost = false
		return verification.Job{}, errors.New("SECRET lost claim reply")
	}
	return job, err
}

func TestVerificationRecoveryLostClaimAcknowledgementDoesNotDispatch(t *testing.T) {
	f := newWorkerFixture(t)
	f.registerIntent(t)
	_, err := f.storage.EnqueueVerificationJob(f.ctx, f.request.Job)
	require.NoError(t, err)
	wrapped := &recoveryClaimStore{Store: f.storage, lost: true}
	e := f.engine(t, wrapped)
	request := recoveryRequest()
	request.LeaseDuration = 2 * time.Second
	page, err := e.RecoverVerificationPage(f.ctx, request)
	require.NoError(t, err)
	require.Equal(t, engine.RecoveryUnavailable, page.Items[0].Status)
	require.Zero(t, f.adapter.evaluations)
	saved, err := f.storage.GetVerificationJob(f.ctx, f.request.Job.ID)
	require.NoError(t, err)
	require.Equal(t, 1, saved.Attempts)
	page, err = e.RecoverVerificationPage(f.ctx, request)
	require.NoError(t, err)
	require.Equal(t, engine.RecoveryBusy, page.Items[0].Status)
	require.Zero(t, f.adapter.evaluations)
	require.Eventually(t, func() bool { return time.Now().After(saved.Lease.ExpiresAt) }, 5*time.Second, 10*time.Millisecond)
	page, err = e.RecoverVerificationPage(f.ctx, request)
	require.NoError(t, err)
	require.Equal(t, engine.RecoveryCommitted, page.Items[0].Status)
	require.Equal(t, 1, f.adapter.evaluations)
}

func TestVerificationRecoveryLostCommitAcknowledgementIsTerminal(t *testing.T) {
	f := newWorkerFixture(t)
	f.registerIntent(t)
	_, err := f.storage.EnqueueVerificationJob(f.ctx, f.request.Job)
	require.NoError(t, err)
	commits := 0
	f.config.Fence = workerFence(func(_ context.Context, b verification.Binding, commit func(verification.Binding) error) error {
		commits++
		if commitErr := commit(b); commitErr != nil {
			return commitErr
		}
		return errors.New("SECRET lost commit reply")
	})
	e := f.engine(t, f.storage)
	page, err := e.RecoverVerificationPage(f.ctx, recoveryRequest())
	require.NoError(t, err)
	require.Equal(t, engine.RecoveryUnavailable, page.Items[0].Status)
	page, err = e.RecoverVerificationPage(f.ctx, recoveryRequest())
	require.NoError(t, err)
	require.Equal(t, engine.RecoveryTerminal, page.Items[0].Status)
	require.Equal(t, verification.JobCommitted, page.Items[0].State)
	require.Equal(t, 1, f.adapter.evaluations)
	require.Equal(t, 1, commits)
}

func TestVerificationRecoveryExpiresOverdueQueuedWork(t *testing.T) {
	f := newWorkerFixture(t)
	f.request.Job.Deadline = time.Now().Add(2 * time.Second)
	f.registerIntent(t)
	_, err := f.storage.EnqueueVerificationJob(f.ctx, f.request.Job)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return time.Now().After(f.request.Job.Deadline) }, 5*time.Second, 10*time.Millisecond)
	page, err := f.engine(t, f.storage).RecoverVerificationPage(f.ctx, recoveryRequest())
	require.NoError(t, err)
	require.Equal(t, engine.RecoveryExpired, page.Items[0].Status)
	require.Equal(t, verification.JobInconclusive, page.Items[0].State)
	require.Zero(t, f.adapter.evaluations)
	saved, err := f.storage.GetRun(f.ctx, f.request.Job.Binding.RunID)
	require.NoError(t, err)
	require.Equal(t, verification.StatusInconclusive, saved.VerificationStatus)
}

func TestVerificationRecoveryRequiresConfiguredCapabilities(t *testing.T) {
	f := newWorkerFixture(t)
	e, err := engine.New(engine.WithStore(f.storage))
	require.NoError(t, err)
	_, err = e.RecoverVerificationPage(f.ctx, recoveryRequest())
	require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
	withoutIntents := struct{ workerRequiredStore }{f.storage}
	_, err = f.engine(t, withoutIntents).RecoverVerificationPage(f.ctx, recoveryRequest())
	require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
}
