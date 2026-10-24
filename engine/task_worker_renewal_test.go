package engine_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

type engineRenewalSource func(context.Context, coordination.Claim, time.Duration) (coordination.TaskRenewalRequest, error)

func (f engineRenewalSource) ResolveTaskRenewal(ctx context.Context, c coordination.Claim, d time.Duration) (coordination.TaskRenewalRequest, error) {
	return f(ctx, c, d)
}

type engineRenewalFence func(context.Context, coordination.TaskRenewalRequest, func(coordination.TaskRenewalRequest) error) error

func (f engineRenewalFence) WithTaskRenewalFence(ctx context.Context, r coordination.TaskRenewalRequest, next func(coordination.TaskRenewalRequest) error) error {
	return f(ctx, r, next)
}

type engineRenewalAuthorizer func(context.Context, coordination.TaskRenewalRequest) error

func (f engineRenewalAuthorizer) AuthorizeTaskRenewal(ctx context.Context, r coordination.TaskRenewalRequest) error {
	return f(ctx, r)
}

type engineRenewer func(context.Context, coordination.TaskRenewalRequest) (coordination.TaskRenewal, error)

func (f engineRenewer) Renew(ctx context.Context, r coordination.TaskRenewalRequest) (coordination.TaskRenewal, error) {
	return f(ctx, r)
}

func TestSchedulerTaskWorkerRenewsDurableOwnership(t *testing.T) {
	f := newTaskClaimRuntimeFixture(t, false)
	f.request.Acquisition.Claim.Duration = 3 * time.Second
	cfg := schedulerTaskWorkerConfig(t, f)
	renewer, err := coordination.NewTaskClaimRenewer(f.f.storage, engineRenewalFence(func(_ context.Context, r coordination.TaskRenewalRequest, next func(coordination.TaskRenewalRequest) error) error {
		return next(r)
	}), engineRenewalAuthorizer(func(context.Context, coordination.TaskRenewalRequest) error { return nil }))
	require.NoError(t, err)
	var renewals atomic.Int32
	acknowledged := make(chan struct{})
	var acknowledge sync.Once
	var originalExpiry time.Time
	admitter := cfg.Admitter
	cfg.Admitter = schedulerClaimAdmitterForRenewal(func(ctx context.Context, r coordination.ReadyTaskClaimRequest) (coordination.ReadyTaskClaim, error) {
		result, admitErr := admitter.Admit(ctx, r)
		originalExpiry = result.Acquisition.Claim.ExpiresAt
		return result, admitErr
	})
	cfg.Renewal = &coordination.TaskRenewalConfig{Duration: 6 * time.Second, Interval: 2 * time.Second,
		Source: engineRenewalSource(func(ctx context.Context, c coordination.Claim, d time.Duration) (coordination.TaskRenewalRequest, error) {
			parent, readErr := f.f.storage.GetRun(ctx, f.options.Assignment.Handoff.ParentRunID)
			if readErr != nil {
				return coordination.TaskRenewalRequest{}, readErr
			}
			child, readErr := f.f.storage.GetRun(ctx, c.RunID)
			if readErr != nil {
				return coordination.TaskRenewalRequest{}, readErr
			}
			assignment, readErr := f.f.storage.GetAssignment(ctx, f.options.Assignment.Handoff.AssignmentID)
			if readErr != nil {
				return coordination.TaskRenewalRequest{}, readErr
			}
			if child.State != run.StateRunning {
				return coordination.TaskRenewalRequest{}, errors.New("child not running")
			}
			return coordination.TaskRenewalRequest{SchemaVersion: 1, Claim: c, Actor: c.Worker.Principal, Reason: "continue owned task", ParentRunID: parent.ID, ParentRunRevision: parent.Revision, RunRevision: child.Revision, AssignmentID: assignment.Handoff.AssignmentID, AssignmentRevision: assignment.Revision, AssignmentDigest: assignment.Digest, Duration: d}, nil
		}),
		Renewer: engineRenewer(func(ctx context.Context, r coordination.TaskRenewalRequest) (coordination.TaskRenewal, error) {
			result, renewErr := renewer.Renew(ctx, r)
			if renewErr == nil && result.Fresh {
				renewals.Add(1)
				acknowledge.Do(func() { close(acknowledged) })
			}
			return result, renewErr
		}),
	}
	// This provider fixture uses an explicit permissive call guard. Production
	// call leases retain their own expiry, even when ownership is renewed.
	f.client.before = func() {
		select {
		case <-acknowledged:
		case <-time.After(10 * time.Second):
			t.Error("renewal not acknowledged")
			return
		}
		if remaining := time.Until(originalExpiry.Add(100 * time.Millisecond)); remaining > 0 {
			time.Sleep(remaining)
		}
	}
	scheduler, err := coordination.NewScheduler(cfg)
	require.NoError(t, err)
	require.NoError(t, scheduler.Tick(f.f.ctx, f.request.Acquisition.Claim.PlanID))
	require.Positive(t, renewals.Load())
	require.True(t, time.Now().After(originalExpiry))
	result, err := f.f.storage.GetRun(f.f.ctx, f.dispatch.RunID)
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, result.VerificationStatus)
	require.Equal(t, "done", result.Output)
	claim, err := f.f.storage.GetTaskClaim(f.f.ctx, f.request.Acquisition.Claim.PlanID, f.request.Acquisition.Claim.TaskID)
	require.NoError(t, err)
	require.Greater(t, claim.Revision, int64(2))
	require.Equal(t, coordination.ClaimDispatching, claim.State)
	budget, err := f.f.storage.GetBudgetSnapshot(f.f.ctx, claim.PlanID)
	require.NoError(t, err)
	require.Equal(t, f.request.Acquisition.Reservation.Allocation, budget.Charged)
	require.NoError(t, scheduler.Shutdown(context.Background()))
}

type schedulerClaimAdmitterForRenewal func(context.Context, coordination.ReadyTaskClaimRequest) (coordination.ReadyTaskClaim, error)

func (f schedulerClaimAdmitterForRenewal) Admit(ctx context.Context, r coordination.ReadyTaskClaimRequest) (coordination.ReadyTaskClaim, error) {
	return f(ctx, r)
}
