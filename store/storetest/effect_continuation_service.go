package storetest

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

type continuationResolver func(context.Context, coordination.Claim, execution.RunRecoverySnapshot, execution.EffectRecord, []execution.EffectContinuation) (*coordination.EffectContinuationOffer, error)

func (f continuationResolver) ResolveEffectContinuation(ctx context.Context, c coordination.Claim, s execution.RunRecoverySnapshot, e execution.EffectRecord, h []execution.EffectContinuation) (*coordination.EffectContinuationOffer, error) {
	return f(ctx, c, s, e, h)
}

type managedContinuationAdapter struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (a *managedContinuationAdapter) Execute(ctx context.Context, r execution.EffectRequest, _ []byte) (execution.EffectReceipt, error) {
	a.calls.Add(1)
	if a.entered != nil {
		close(a.entered)
		<-ctx.Done()
		<-a.release
		return execution.EffectReceipt{}, ctx.Err()
	}
	return continuationReceipt(r, false), nil
}
func (*managedContinuationAdapter) Reconcile(_ context.Context, r execution.EffectRequest) (execution.EffectStatus, error) {
	receipt := continuationReceipt(r, false)
	return execution.EffectStatus{Outcome: receipt.Outcome, Receipt: &receipt}, nil
}

func testManagedEffectContinuation(t *testing.T, backend store.Store, driver assurance.Driver) {
	for _, mode := range []string{"restart", "revoked scan", "revoked after snapshot", "revoked action", "page timeout", "nil pending", "drain"} {
		t.Run(mode, func(t *testing.T) {
			f, r := effectContinuationFixture(t, backend, driver, execution.EffectNonIdempotent, false)
			adapter := &managedContinuationAdapter{}
			if mode == "drain" {
				adapter.entered = make(chan struct{})
				adapter.release = make(chan struct{})
			}
			var scanned, resolved atomic.Int32
			var revoked atomic.Bool
			config := coordination.EffectContinuationServiceConfig{Effects: execution.EffectExecutorConfig{Store: f.repo, Adapters: []execution.EffectRegistration{{Requirement: r.Operation.Requirement, Adapter: adapter}}, MaxConcurrent: 1, Authorize: func(context.Context, execution.EffectRequest, execution.EffectAction) error { return nil }, AuthorizeScan: func(context.Context, execution.RunCallSeal) error {
				scanned.Add(1)
				if mode == "revoked scan" || revoked.Load() {
					return execution.ErrEffectUnavailable
				}
				return nil
			}}, Authority: continuationAuthority{}, Resolver: continuationResolver(func(ctx context.Context, _ coordination.Claim, _ execution.RunRecoverySnapshot, _ execution.EffectRecord, _ []execution.EffectContinuation) (*coordination.EffectContinuationOffer, error) {
				resolved.Add(1)
				if mode == "page timeout" {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				if mode == "nil pending" {
					return nil, nil
				}
				return &coordination.EffectContinuationOffer{Request: r, Arguments: []byte("payload")}, nil
			}), Discovery: adoptionPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
				return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: r.Original.Scope, PlanID: r.Reservation.PlanID}}}, nil
			}), Interval: time.Second, MaxInFlight: 1}
			if mode == "page timeout" {
				config.PageTimeout = 10 * time.Millisecond
			}
			if mode == "revoked action" {
				config.Authority = continuationAuthority{authorize: func(execution.EffectAction) error { return execution.ErrEffectUnavailable }}
			}
			if mode == "revoked after snapshot" {
				config.Effects.Store = continuationScanRevocationStore{Repository: f.repo, revoked: &revoked}
			}
			managed, err := engine.New(engine.WithStore(backend), engine.WithEffectContinuation(config), engine.WithSuspensionTTL(0))
			require.NoError(t, err)
			require.ErrorIs(t, engine.WithEffectContinuation(config)(managed), coordination.ErrInvalidScheduler)
			require.NoError(t, managed.Start(f.ctx))
			t.Cleanup(func() { require.NoError(t, managed.Stop(context.Background())) })
			if mode == "drain" {
				select {
				case <-adapter.entered:
				case <-time.After(10 * time.Second):
					t.Fatal("adapter did not start")
				}
				stopped, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()
				require.ErrorIs(t, managed.Stop(stopped), context.DeadlineExceeded)
				status, ok := managed.EffectContinuationStatus()
				require.True(t, ok)
				require.True(t, status.Stopping)
				require.Positive(t, status.Active)
				close(adapter.release)
				require.NoError(t, managed.Stop(context.Background()))
				retained, readErr := f.repo.GetEffectContinuationResult(f.ctx, r)
				require.NoError(t, readErr)
				require.Nil(t, retained)
				account, readErr := f.repo.GetBudgetSnapshot(f.ctx, r.Reservation.PlanID)
				require.NoError(t, readErr)
				require.EqualValues(t, 1100, account.Charged.USDMicros)
				return
			}
			if mode == "revoked scan" {
				require.Eventually(t, func() bool { return scanned.Load() > 0 }, 5*time.Second, time.Millisecond)
				require.NoError(t, managed.Stop(context.Background()))
				require.Zero(t, resolved.Load())
				require.Zero(t, adapter.calls.Load())
				return
			}
			if mode == "revoked action" || mode == "page timeout" || mode == "revoked after snapshot" {
				require.Eventually(t, func() bool { status, _ := managed.EffectContinuationStatus(); return status.Failures > 0 }, 5*time.Second, time.Millisecond)
				require.NoError(t, managed.Stop(context.Background()))
				require.Zero(t, adapter.calls.Load())
				if mode == "revoked after snapshot" {
					require.Zero(t, resolved.Load())
				}
				return
			}
			if mode == "nil pending" {
				require.Eventually(t, func() bool { return resolved.Load() > 0 }, 5*time.Second, time.Millisecond)
				require.NoError(t, managed.Stop(context.Background()))
				require.Zero(t, adapter.calls.Load())
				return
			}
			require.Eventually(t, func() bool {
				result, readErr := f.repo.GetEffectContinuationResult(f.ctx, r)
				return readErr == nil && result != nil
			}, 10*time.Second, 10*time.Millisecond)
			require.NoError(t, managed.Stop(context.Background()))
			require.EqualValues(t, 1, adapter.calls.Load())
			previous := resolved.Load()
			require.NoError(t, managed.Start(f.ctx))
			require.Eventually(t, func() bool { return resolved.Load() > previous }, 5*time.Second, time.Millisecond)
			require.NoError(t, managed.Stop(context.Background()))
			require.EqualValues(t, 1, adapter.calls.Load(), "restart observes retained attempt without replay")
			status, ok := managed.EffectContinuationStatus()
			require.True(t, ok)
			require.False(t, status.Running)
			require.Zero(t, status.Active)
		})
	}
}

type continuationScanRevocationStore struct {
	*assurance.Repository
	revoked *atomic.Bool
}

func (s continuationScanRevocationStore) SnapshotRunRecovery(ctx context.Context, seal execution.RunCallSeal) (execution.RunRecoverySnapshot, error) {
	snapshot, err := s.Repository.SnapshotRunRecovery(ctx, seal)
	s.revoked.Store(true)
	return snapshot, err
}
