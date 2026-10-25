package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/store/internal/assurance"
)

func coordinatorFixture(t *testing.T, s *Store) (context.Context, orchestration.BudgetPolicy, orchestration.CoordinatorRequest) {
	t.Helper()
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "coordinator"}}}
	ctx := cortex.WithScope(t.Context(), scope)
	allocation := execution.Resources{Tokens: 100, FanOut: 1}
	policy := orchestration.BudgetPolicy{SchemaVersion: 1, ExecutionID: id.NewOrchestrationID(), PreparedDigest: evidence.Digest([]byte("team")), Scope: scope, Limits: allocation, Participants: map[string]execution.Resources{"worker": allocation}, Deadline: time.Now().Add(time.Hour), TokenOnly: true}
	require.NoError(t, s.CreateOrchestrationBudget(ctx, policy))
	require.NoError(t, s.CreateOrchestrationRun(ctx, &orchestration.Run{ID: policy.ExecutionID, Status: orchestration.StatusRunning, ContinuationGeneration: 1, Input: "work", StartedAt: time.Now()}))
	request := orchestration.CoordinatorRequest{TeamID: policy.ExecutionID, Owner: id.New(orchestration.PrefixCoordinator), ExpectedGeneration: 1, Initial: true, Duration: time.Minute}
	return ctx, policy, request
}

func TestCoordinatorLostAcknowledgmentAndExpiredTakeover(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "coordinator.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	s := openTestStore(t, dsn)
	ctx, policy, request := coordinatorFixture(t, s)
	for _, rollback := range []bool{true, false} {
		s.assuranceStore = assurance.New(teamBudgetFaultDriver{assuranceDriver: assuranceDriver{store: s}, rollback: rollback})
		uncertain, err := s.AcquireOrchestrationCoordinator(ctx, request)
		require.ErrorIs(t, err, errTeamBudgetFault)
		require.Zero(t, uncertain)
		require.NoError(t, s.Close())
		s = openTestStore(t, dsn)
		parent, readErr := s.GetOrchestrationRun(ctx, policy.ExecutionID)
		require.NoError(t, readErr)
		if rollback {
			require.Equal(t, int64(1), parent.ContinuationGeneration)
			_, readErr = s.GetOrchestrationCoordinator(ctx, policy.ExecutionID)
			require.ErrorIs(t, readErr, cortex.ErrOrchestrationCoordinatorNotFound)
		} else {
			require.Equal(t, int64(2), parent.ContinuationGeneration)
		}
	}
	prior, err := s.GetOrchestrationCoordinator(ctx, policy.ExecutionID)
	require.NoError(t, err)
	_, err = s.AcquireOrchestrationCoordinator(ctx, request)
	require.ErrorIs(t, err, cortex.ErrOrchestrationRunConflict)
	s.assuranceStore = assurance.New(assuranceDriver{store: s}).WithClock(teamBudgetClock{at: prior.ExpiresAt})
	_, err = s.CheckOrchestrationCoordinator(ctx, prior)
	require.ErrorIs(t, err, cortex.ErrOrchestrationRunConflict)
	_, err = s.RenewOrchestrationCoordinator(ctx, prior, time.Minute)
	require.ErrorIs(t, err, cortex.ErrOrchestrationRunConflict)
	request.Initial = false
	request.Owner = id.New(orchestration.PrefixCoordinator)
	request.ExpectedGeneration = prior.Generation
	winner, err := s.AcquireOrchestrationCoordinator(ctx, request)
	require.NoError(t, err)
	require.Equal(t, prior.Generation+1, winner.Lease.Generation)
	stale := *winner.Run
	stale.ContinuationGeneration = prior.Generation
	stale.Status = orchestration.StatusFailed
	require.ErrorIs(t, s.FinishOrchestrationCoordinator(ctx, prior, &stale), cortex.ErrOrchestrationRunConflict)
	account, err := s.GetOrchestrationBudgetSnapshot(ctx, policy.ExecutionID)
	require.NoError(t, err)
	require.False(t, account.Closed, "stale completion must not close the successor's account")
	intent := orchestration.InvocationIntent{SchemaVersion: 1, OrchestrationID: policy.ExecutionID, Scope: policy.Scope, Slot: "sequential/0", AgentName: "worker", RequestDigest: evidence.Digest([]byte("work"))}
	require.ErrorIs(t, s.BeginCoordinatedInvocation(ctx, prior, intent), cortex.ErrOrchestrationRunConflict)
	require.NoError(t, s.BeginCoordinatedInvocation(ctx, winner.Lease, intent))
}

func TestCoordinatorFinishRollbackAndLostAcknowledgment(t *testing.T) {
	s := newTestStore(t)
	ctx, policy, request := coordinatorFixture(t, s)
	claim, err := s.AcquireOrchestrationCoordinator(ctx, request)
	require.NoError(t, err)
	claim.Run.Status = orchestration.StatusCompleted
	claim.Run.Output = "done"
	s.assuranceStore = assurance.New(teamBudgetFaultDriver{assuranceDriver: assuranceDriver{store: s}, rollback: true})
	require.ErrorIs(t, s.FinishOrchestrationCoordinator(ctx, claim.Lease, claim.Run), errTeamBudgetFault)
	s.assuranceStore = assurance.New(assuranceDriver{store: s})
	parent, err := s.GetOrchestrationRun(ctx, policy.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, orchestration.StatusRunning, parent.Status)
	account, err := s.GetOrchestrationBudgetSnapshot(ctx, policy.ExecutionID)
	require.NoError(t, err)
	require.False(t, account.Closed)
	s.assuranceStore = assurance.New(teamBudgetFaultDriver{assuranceDriver: assuranceDriver{store: s}})
	require.ErrorIs(t, s.FinishOrchestrationCoordinator(ctx, claim.Lease, claim.Run), errTeamBudgetFault)
	s.assuranceStore = assurance.New(assuranceDriver{store: s})
	parent, err = s.GetOrchestrationRun(ctx, policy.ExecutionID)
	require.NoError(t, err)
	require.Equal(t, orchestration.StatusCompleted, parent.Status)
	require.Equal(t, "done", parent.Output)
	account, err = s.GetOrchestrationBudgetSnapshot(ctx, policy.ExecutionID)
	require.NoError(t, err)
	require.True(t, account.Closed)
}

func TestCoordinatorClockCannotMoveBeforeAcknowledgedOwnership(t *testing.T) {
	s := newTestStore(t)
	ctx, _, request := coordinatorFixture(t, s)
	claim, err := s.AcquireOrchestrationCoordinator(ctx, request)
	require.NoError(t, err)
	s.assuranceStore = assurance.New(assuranceDriver{store: s}).WithClock(teamBudgetClock{at: claim.Lease.AcquiredAt.Add(-time.Millisecond)})
	_, err = s.CheckOrchestrationCoordinator(ctx, claim.Lease)
	require.ErrorIs(t, err, cortex.ErrOrchestrationRunConflict)
	_, err = s.RenewOrchestrationCoordinator(ctx, claim.Lease, time.Minute)
	require.ErrorIs(t, err, cortex.ErrOrchestrationRunConflict)
}

func TestCoordinatorReservationRollbackAndLostAcknowledgment(t *testing.T) {
	s := newTestStore(t)
	ctx, policy, request := coordinatorFixture(t, s)
	claim, err := s.AcquireOrchestrationCoordinator(ctx, request)
	require.NoError(t, err)
	reservation := execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: policy.ExecutionID, PlanDigest: policy.PreparedDigest, TaskID: id.NewAgentRunID(), Scope: policy.Scope, OwnerRef: "worker", Kind: execution.ReservationLeaf, Allocation: policy.Participants["worker"], Deadline: policy.Deadline}
	for _, rollback := range []bool{true, false} {
		s.assuranceStore = assurance.New(teamBudgetFaultDriver{assuranceDriver: assuranceDriver{store: s}, rollback: rollback})
		unknown, reserveErr := s.ReserveCoordinatedChild(ctx, claim.Lease, reservation)
		require.ErrorIs(t, reserveErr, errTeamBudgetFault)
		require.Zero(t, unknown)
		s.assuranceStore = assurance.New(assuranceDriver{store: s})
		parent, readErr := s.GetOrchestrationRun(ctx, policy.ExecutionID)
		require.NoError(t, readErr)
		account, readErr := s.GetOrchestrationBudgetSnapshot(ctx, policy.ExecutionID)
		require.NoError(t, readErr)
		if rollback {
			require.Empty(t, parent.AgentRunIDs)
			require.Zero(t, account.Reservations)
			_, readErr = s.GetReservation(ctx, reservation.ID)
			require.ErrorIs(t, readErr, execution.ErrReservationNotFound)
		} else {
			require.Equal(t, []id.AgentRunID{reservation.TaskID}, parent.AgentRunIDs)
			require.Equal(t, 1, account.Reservations)
			_, readErr = s.ReserveCoordinatedChild(ctx, claim.Lease, reservation)
			require.ErrorIs(t, readErr, cortex.ErrAlreadyExists)
		}
	}
}

func TestCoordinatorRejectsBackwardClockAfterRelease(t *testing.T) {
	s := newTestStore(t)
	ctx, _, request := coordinatorFixture(t, s)
	claim, err := s.AcquireOrchestrationCoordinator(ctx, request)
	require.NoError(t, err)
	claim.Run.Status = orchestration.StatusWaiting
	require.NoError(t, s.FinishOrchestrationCoordinator(ctx, claim.Lease, claim.Run))
	s.assuranceStore = assurance.New(assuranceDriver{store: s}).WithClock(teamBudgetClock{at: claim.Lease.AcquiredAt.Add(-time.Millisecond)})
	request.Initial = false
	request.Owner = id.New(orchestration.PrefixCoordinator)
	request.ExpectedGeneration = claim.Lease.Generation
	unknown, err := s.AcquireOrchestrationCoordinator(ctx, request)
	require.ErrorIs(t, err, cortex.ErrOrchestrationRunConflict)
	require.Zero(t, unknown)
}
