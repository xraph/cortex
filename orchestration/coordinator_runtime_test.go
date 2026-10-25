package orchestration

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

type coordinatorRuntimeStore struct {
	CoordinatorStore
	renewCall func(context.Context, CoordinatorLease, time.Duration) (CoordinatorLease, error)
}

func (s coordinatorRuntimeStore) RenewOrchestrationCoordinator(ctx context.Context, l CoordinatorLease, d time.Duration) (CoordinatorLease, error) {
	return s.renewCall(ctx, l, d)
}
func (s coordinatorRuntimeStore) CheckOrchestrationCoordinator(_ context.Context, l CoordinatorLease) (CoordinatorLease, error) {
	return l, nil
}

func runtimeLease() CoordinatorLease {
	now := time.Now().UTC()
	return CoordinatorLease{SchemaVersion: 1, TeamID: id.NewOrchestrationID(), Owner: id.New(PrefixCoordinator), Scope: cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "runtime"}}}, Generation: 2, Revision: 1, AcquiredAt: now, ExpiresAt: now.Add(300 * time.Millisecond)}
}

func TestCoordinatorWatchdogCancelsBlockedRenewal(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	storage := coordinatorRuntimeStore{renewCall: func(context.Context, CoordinatorLease, time.Duration) (CoordinatorLease, error) {
		close(entered)
		<-release
		return CoordinatorLease{}, errors.New("lost renewal")
	}}
	ctx, pass := startCoordinatorPass(t.Context(), storage, runtimeLease())
	defer pass.stop()
	defer close(release)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("renewal did not start")
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog waited for blocked storage")
	}
	require.ErrorIs(t, context.Cause(ctx), cortex.ErrOrchestrationRunConflict)
	require.Error(t, CheckCoordinator(ctx))
}

func TestCoordinatorRenewalAcknowledgmentExtendsWatchdog(t *testing.T) {
	lease := runtimeLease()
	storage := coordinatorRuntimeStore{renewCall: func(ctx context.Context, l CoordinatorLease, _ time.Duration) (CoordinatorLease, error) {
		if l.Revision > 1 {
			<-ctx.Done()
			return CoordinatorLease{}, ctx.Err()
		}
		_, l.PreviousDigest, _ = l.Freeze()
		l.Revision++
		l.ExpiresAt = time.Now().Add(2 * time.Second)
		return l, nil
	}}
	ctx, pass := startCoordinatorPass(t.Context(), storage, lease)
	defer pass.stop()
	require.Eventually(t, func() bool { l, err := pass.receipt(); return err == nil && l.Revision == 2 }, time.Second, 10*time.Millisecond)
	timer := time.NewTimer(time.Until(lease.ExpiresAt.Add(30 * time.Millisecond)))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal("old expiry canceled renewed pass")
	}
	require.NoError(t, CheckCoordinator(ctx))
}

func TestCoordinatorLostRenewalAcknowledgmentStopsAdmission(t *testing.T) {
	lost := errors.New("renewal outcome unknown")
	storage := coordinatorRuntimeStore{renewCall: func(context.Context, CoordinatorLease, time.Duration) (CoordinatorLease, error) {
		return CoordinatorLease{}, lost
	}}
	ctx, pass := startCoordinatorPass(t.Context(), storage, runtimeLease())
	defer pass.stop()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("uncertain renewal retained admission")
	}
	require.ErrorIs(t, CheckCoordinator(ctx), lost)
	_, err := pass.receipt()
	require.ErrorIs(t, err, lost)
}

func TestCoordinatorRenewalAtTeamDeadlineDoesNotSpin(t *testing.T) {
	var calls atomic.Int32
	storage := coordinatorRuntimeStore{renewCall: func(_ context.Context, l CoordinatorLease, _ time.Duration) (CoordinatorLease, error) {
		calls.Add(1)
		_, l.PreviousDigest, _ = l.Freeze()
		l.Revision++
		return l, nil
	}}
	ctx, pass := startCoordinatorPass(t.Context(), storage, runtimeLease())
	defer pass.stop()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("deadline did not cancel pass")
	}
	require.EqualValues(t, 1, calls.Load(), "renewal cannot extend the immutable account deadline")
}
