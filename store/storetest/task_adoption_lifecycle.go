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

type adoptionObservedStore struct {
	coordination.TaskAdoptionStore
	reads atomic.Int32
}

func (s *adoptionObservedStore) ListTaskClaims(ctx context.Context, r coordination.ClaimListRequest) ([]coordination.Claim, error) {
	claims, err := s.TaskAdoptionStore.ListTaskClaims(ctx, r)
	s.reads.Add(1)
	return claims, err
}

type adoptionPlanSource func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error)

func (f adoptionPlanSource) ListScheduledPlans(ctx context.Context, r coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
	return f(ctx, r)
}

type adoptionResolver func(context.Context, coordination.Claim, execution.RunRecoverySnapshot) (*coordination.TaskAcceptanceRequest, error)

func (f adoptionResolver) ResolveTaskRecovery(ctx context.Context, c coordination.Claim, s execution.RunRecoverySnapshot) (*coordination.TaskAcceptanceRequest, error) {
	return f(ctx, c, s)
}

type adoptionFence struct{}

func (adoptionFence) WithTaskRecoveryFence(_ context.Context, r coordination.TaskAcceptanceRequest, next func(coordination.TaskAcceptanceRequest) error) error {
	return next(r)
}

type adoptionAuthorizer struct{}

func (adoptionAuthorizer) AuthorizeTaskRecovery(context.Context, coordination.TaskAcceptanceRequest) error {
	return nil
}

func testManagedTaskAdoption(t *testing.T, backend store.Store, driver assurance.Driver) {
	t.Helper()
	f := newTaskRecoveryFixture(t, backend, driver, "")
	claim, err := f.repo.GetTaskClaim(f.ctx, f.request.Claim.PlanID, f.request.Claim.TaskID)
	require.NoError(t, err)
	original, err := backend.GetRun(f.ctx, claim.RunID)
	require.NoError(t, err)
	var resolutions atomic.Int32
	observed := &adoptionObservedStore{TaskAdoptionStore: f.repo}
	cfg := coordination.TaskAdoptionServiceConfig{Store: observed, Interval: 10 * time.Millisecond, PageSize: 64,
		Discovery: adoptionPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: claim.Scope, PlanID: claim.PlanID}}}, nil
		}),
		AuthorizeScan: func(context.Context, coordination.Claim, execution.RunCallSeal) error { return nil }, Fence: adoptionFence{}, Authorizer: adoptionAuthorizer{},
		Resolver: adoptionResolver(func(_ context.Context, current coordination.Claim, snapshot execution.RunRecoverySnapshot) (*coordination.TaskAcceptanceRequest, error) {
			resolutions.Add(1)
			require.Equal(t, claim.Digest, current.Digest)
			require.Equal(t, f.request.Recovery.SnapshotDigest, snapshot.Digest)
			request, _, freezeErr := f.request.Freeze()
			return &request, freezeErr
		}),
	}
	// These local ports accept fixture proof. The test qualifies lifecycle and
	// storage integration, not provider identity or permanent remote quiescence.
	e, err := engine.New(engine.WithStore(backend), engine.WithTaskAdoption(cfg), engine.WithSuspensionTTL(0))
	require.NoError(t, err)
	startup, cancel := context.WithCancel(f.ctx)
	require.NoError(t, e.Start(startup))
	cancel()
	t.Cleanup(func() { require.NoError(t, e.Stop(context.Background())) })
	require.Eventually(t, func() bool {
		current, readErr := f.repo.GetTaskClaim(f.ctx, claim.PlanID, claim.TaskID)
		return readErr == nil && current.State == coordination.ClaimCompleted
	}, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, e.Stop(context.Background()))
	accepted, err := f.repo.GetTaskAcceptance(f.ctx, f.request.ID)
	require.NoError(t, err)
	require.NoError(t, accepted.Validate())
	require.Equal(t, claim.ExpiresAt, accepted.Claim.ExpiresAt)
	result, err := backend.GetRun(f.ctx, claim.RunID)
	require.NoError(t, err)
	require.Equal(t, original, result)
	call, err := f.repo.GetRunCall(f.ctx, f.model.RunID, f.model.OperationDigest)
	require.NoError(t, err)
	require.Nil(t, call.Completion)
	status, configured := e.TaskAdoptionStatus()
	require.True(t, configured)
	require.False(t, status.Running)
	require.Zero(t, status.Active)
	require.Zero(t, status.Failures)
	require.Equal(t, int32(1), resolutions.Load())
	before := observed.reads.Load()
	require.NoError(t, e.Start(f.ctx))
	require.Eventually(t, func() bool { return observed.reads.Load() > before }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, e.Stop(context.Background()))
	require.Equal(t, int32(1), resolutions.Load())
}
