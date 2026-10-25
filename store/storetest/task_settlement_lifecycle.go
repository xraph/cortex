package storetest

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

type settlementLifecycleStore struct {
	coordination.TaskSettlementStore
	coordination.TaskCallSealStore
	attempts     atomic.Int32
	loseFirstAck bool
}

func (s *settlementLifecycleStore) SettleTaskUsage(ctx context.Context, r coordination.TaskUsageSettlementRequest) (coordination.TaskUsageSettlement, error) {
	receipt, err := s.TaskSettlementStore.SettleTaskUsage(ctx, r)
	if err != nil {
		return receipt, err
	}
	if s.attempts.Add(1) == 1 && s.loseFirstAck {
		return coordination.TaskUsageSettlement{}, errAssignmentInjected
	}
	return receipt, nil
}

type settlementLifecycleResolver func(context.Context, coordination.TaskUsageSnapshot, coordination.TaskUsageClosure, execution.Reservation) (*coordination.TaskUsageSettlementRequest, error)

func (f settlementLifecycleResolver) ResolveTaskUsageSettlement(ctx context.Context, s coordination.TaskUsageSnapshot, c coordination.TaskUsageClosure, r execution.Reservation) (*coordination.TaskUsageSettlementRequest, error) {
	return f(ctx, s, c, r)
}

type settlementLifecycleAuthority struct{}

func (settlementLifecycleAuthority) WithTaskCallSealFence(_ context.Context, r coordination.TaskCallSealRequest, next func(coordination.TaskCallSealRequest) error) error {
	return next(r)
}
func (settlementLifecycleAuthority) AuthorizeTaskCallSeal(context.Context, coordination.TaskCallSealRequest) error {
	return nil
}

func (settlementLifecycleAuthority) WithTaskUsageClosureFence(_ context.Context, r coordination.TaskUsageClosureRequest, next func(coordination.TaskUsageClosureRequest) error) error {
	return next(r)
}
func (settlementLifecycleAuthority) AuthorizeTaskUsageClosure(context.Context, coordination.TaskUsageClosureRequest) error {
	return nil
}
func (settlementLifecycleAuthority) WithTaskUsageSettlementFence(_ context.Context, r coordination.TaskUsageSettlementRequest, next func(coordination.TaskUsageSettlementRequest) error) error {
	return next(r)
}
func (settlementLifecycleAuthority) AuthorizeTaskUsageSettlement(context.Context, coordination.TaskUsageSettlementRequest) error {
	return nil
}

func testManagedTaskSettlement(t *testing.T, backend store.Store, driver assurance.Driver) {
	t.Helper()
	t.Run("recovered", func(t *testing.T) { testManagedTaskSettlementCase(t, backend, driver, false) })
	t.Run("ordinary", func(t *testing.T) { testManagedTaskSettlementCase(t, backend, driver, true) })
}

func testManagedTaskSettlementCase(t *testing.T, backend store.Store, driver assurance.Driver, ordinary bool) {
	t.Helper()
	f := budgetedTaskFixture(t, backend, driver, true, ordinary)
	accept := f.repo.AdoptTaskResult
	if ordinary {
		accept = f.repo.AcceptTaskResult
	}
	accepted, err := accept(f.ctx, f.request)
	require.NoError(t, err)
	original, err := backend.GetRun(f.ctx, accepted.Claim.RunID)
	require.NoError(t, err)
	observed := &settlementLifecycleStore{TaskSettlementStore: f.repo, TaskCallSealStore: f.repo, loseFirstAck: true}
	var resolutions atomic.Int32
	authority := settlementLifecycleAuthority{}
	cfg := coordination.TaskSettlementServiceConfig{Store: observed, ClosureActor: "accounting", Interval: 20 * time.Millisecond,
		Discovery: adoptionPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: accepted.Claim.Scope, PlanID: accepted.Claim.PlanID}}}, nil
		}),
		AuthorizeScan: func(context.Context, coordination.Claim, execution.RunCallSeal) error { return nil }, ClosureFence: authority, ClosureAuthorizer: authority, Fence: authority, Authorizer: authority,
		Resolver: settlementLifecycleResolver(func(_ context.Context, snapshot coordination.TaskUsageSnapshot, closed coordination.TaskUsageClosure, reservation execution.Reservation) (*coordination.TaskUsageSettlementRequest, error) {
			resolutions.Add(1)
			r := coordination.TaskUsageSettlementRequest{SchemaVersion: 1, Usage: coordination.TaskUsageRequest{Claim: snapshot.Claim.Token(), Seal: snapshot.Journal.Seal, SnapshotDigest: snapshot.Journal.Digest}, UsageDigest: snapshot.Digest, ClosureDigest: closed.Digest, ReservationID: reservation.Request.ID, ReservationDigest: reservation.RequestDigest, Actor: "fixture", Reason: "final usage", Billing: coordination.TaskBillingProof{Issuer: "fixture", Revision: "1", ProofRef: "bill", ProofDigest: evidence.Digest([]byte("bill")), PricingRevision: reservation.Request.PricingRevision, ObservedAt: f.clock.Now(), ExpiresAt: f.clock.Now().Add(time.Minute)}}
			return &r, nil
		}),
	}
	if ordinary {
		cfg.CallSealFence, cfg.CallSealAuthorizer = authority, authority
		before, readErr := f.repo.GetRunCallSeal(f.ctx, accepted.Claim.RunID)
		require.NoError(t, readErr)
		require.Nil(t, before)
	}
	// Fixture authority qualifies lifecycle and storage behavior, not live billing.
	eng, err := engine.New(engine.WithStore(backend), engine.WithTaskSettlement(cfg), engine.WithSuspensionTTL(0))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, eng.Stop(context.Background())) })
	require.NoError(t, eng.Start(f.ctx))
	require.Eventually(t, func() bool { return observed.attempts.Load() >= 2 }, 15*time.Second, 10*time.Millisecond)
	require.NoError(t, eng.Stop(context.Background()))
	require.EqualValues(t, 1, resolutions.Load(), "a committed receipt must survive its lost acknowledgment")
	stored, err := f.repo.GetTaskUsageSettlement(f.ctx, accepted.Claim.RunID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.NoError(t, stored.Validate())
	account, err := f.repo.GetBudgetSnapshot(f.ctx, accepted.Claim.PlanID)
	require.NoError(t, err)
	require.EqualValues(t, 18, account.Charged.Tokens)
	require.EqualValues(t, 8, account.Charged.USDMicros)
	after, err := backend.GetRun(f.ctx, accepted.Claim.RunID)
	require.NoError(t, err)
	require.Equal(t, original, after)
	f.clock.advance(time.Hour)
	reopened := assurance.New(driver).WithClock(f.clock)
	restartedStore := &settlementLifecycleStore{TaskSettlementStore: reopened, TaskCallSealStore: reopened}
	cfg.Store = restartedStore
	restarted, err := engine.New(engine.WithStore(backend), engine.WithTaskSettlement(cfg), engine.WithSuspensionTTL(0))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, restarted.Stop(context.Background())) })
	require.NoError(t, restarted.Start(f.ctx))
	require.Eventually(t, func() bool { return restartedStore.attempts.Load() > 0 }, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, restarted.Stop(context.Background()))
	require.EqualValues(t, 1, resolutions.Load())
	status, configured := restarted.TaskSettlementStatus()
	require.True(t, configured)
	require.Zero(t, status.Failures)
	require.Zero(t, status.Active)
}
