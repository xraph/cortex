package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

func budgetedClaimFixture(t *testing.T, backend store.Store) (context.Context, coordination.BudgetedClaimRequest) {
	t.Helper()
	ctx, _, reservation := budgetFixture(t, backend)
	claim := coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: reservation.PlanID, PlanRevision: 1, PlanDigest: reservation.PlanDigest, TaskID: reservation.TaskID, Worker: coordination.Identity{Agent: id.NewAgentID().String(), Node: "node-1", Principal: "worker"}, GrantRef: "grant-1", Duration: time.Minute}
	reservation.OwnerRef = claim.ID.String()
	return ctx, coordination.BudgetedClaimRequest{Claim: claim, Reservation: reservation}
}

// BudgetedClaimTransactions checks atomic ownership and accounting on real stores.
func BudgetedClaimTransactions(t *testing.T, backend store.Store, driver assurance.Driver) {
	plans, ok := driver.(assurance.PlanDriver)
	require.True(t, ok)
	t.Run("acquire_and_retry", func(t *testing.T) {
		ctx, req := budgetedClaimFixture(t, backend)
		repo := assurance.New(driver)
		result, err := repo.AcquireBudgetedTaskClaim(ctx, req)
		require.NoError(t, err)
		require.True(t, result.Fresh)
		require.Equal(t, req.Claim.ID, result.Claim.ID)
		require.Equal(t, req.Reservation.ID, result.Reservation.Request.ID)
		retry, err := repo.AcquireBudgetedTaskClaim(ctx, req)
		require.NoError(t, err)
		result.Fresh = false
		require.Equal(t, result, retry)
		loaded, err := assurance.New(driver).GetBudgetedTaskClaim(ctx, result.Claim.ID)
		require.NoError(t, err)
		require.Equal(t, result, loaded)
		_, err = repo.GetBudgetedTaskClaim(context.Background(), result.Claim.ID)
		require.ErrorIs(t, err, cortex.ErrNoScope)
		_, err = repo.GetBudgetedTaskClaim(ctxWithScope("other"), result.Claim.ID)
		require.ErrorIs(t, err, coordination.ErrClaimNotFound)
		changed := req
		changed.Reservation.ID = id.New(execution.PrefixReservation)
		_, err = repo.AcquireBudgetedTaskClaim(ctx, changed)
		require.ErrorIs(t, err, coordination.ErrClaimConflict)
		snapshot, err := repo.GetBudgetSnapshot(ctx, req.Claim.PlanID)
		require.NoError(t, err)
		require.Equal(t, 1, snapshot.Reservations)
	})
	t.Run("capacity_failure_has_no_claim", func(t *testing.T) {
		ctx, req := budgetedClaimFixture(t, backend)
		repo := assurance.New(driver)
		other := req.Reservation
		other.ID = id.New(execution.PrefixReservation)
		_, err := repo.Reserve(ctx, other)
		require.NoError(t, err)
		result, err := repo.AcquireBudgetedTaskClaim(ctx, req)
		require.ErrorIs(t, err, execution.ErrBudgetExceeded)
		require.Zero(t, result)
		_, err = repo.GetTaskClaim(ctx, req.Claim.PlanID, req.Claim.TaskID)
		require.ErrorIs(t, err, coordination.ErrClaimNotFound)
	})
	for _, kind := range []string{"task_claim_operation", "budget_reservation", "task_claim_budget"} {
		t.Run("rollback_"+kind, func(t *testing.T) {
			ctx, req := budgetedClaimFixture(t, backend)
			faulty := assurance.New(&claimWriteDriver{Driver: driver, plans: plans, failKind: kind})
			result, err := faulty.AcquireBudgetedTaskClaim(ctx, req)
			require.ErrorIs(t, err, errAssignmentInjected)
			require.Zero(t, result)
			repo := assurance.New(driver)
			_, err = repo.GetTaskClaim(ctx, req.Claim.PlanID, req.Claim.TaskID)
			require.ErrorIs(t, err, coordination.ErrClaimNotFound)
			snapshot, err := repo.GetBudgetSnapshot(ctx, req.Claim.PlanID)
			require.NoError(t, err)
			require.Zero(t, snapshot.Reservations)
			_, err = repo.AcquireBudgetedTaskClaim(ctx, req)
			require.NoError(t, err, "all identities must roll back")
		})
	}
	t.Run("lost_acknowledgment", func(t *testing.T) {
		ctx, req := budgetedClaimFixture(t, backend)
		faulty := assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, lostAck: true})
		result, err := faulty.AcquireBudgetedTaskClaim(ctx, req)
		require.ErrorIs(t, err, errAssignmentInjected)
		require.Zero(t, result)
		retry, err := assurance.New(driver).AcquireBudgetedTaskClaim(ctx, req)
		require.NoError(t, err)
		require.False(t, retry.Fresh)
	})
	for _, existing := range []string{"claim", "reservation"} {
		t.Run("reject_independent_"+existing, func(t *testing.T) {
			ctx, req := budgetedClaimFixture(t, backend)
			repo := assurance.New(driver)
			if existing == "claim" {
				_, err := repo.AcquireTaskClaim(ctx, req.Claim)
				require.NoError(t, err)
			} else {
				_, err := repo.Reserve(ctx, req.Reservation)
				require.NoError(t, err)
			}
			result, err := repo.AcquireBudgetedTaskClaim(ctx, req)
			require.Error(t, err)
			require.Zero(t, result)
			if existing == "claim" {
				_, err = repo.GetReservation(ctx, req.Reservation.ID)
				require.ErrorIs(t, err, execution.ErrReservationNotFound)
			} else {
				_, err = repo.GetTaskClaim(ctx, req.Claim.PlanID, req.Claim.TaskID)
				require.ErrorIs(t, err, coordination.ErrClaimNotFound)
			}
		})
	}
	t.Run("settlement_fences_dispatch_and_renewal", func(t *testing.T) {
		ctx, req := budgetedClaimFixture(t, backend)
		repo := assurance.New(driver)
		result, err := repo.AcquireBudgetedTaskClaim(ctx, req)
		require.NoError(t, err)
		require.NoError(t, repo.Settle(ctx, execution.Settlement{ReservationID: req.Reservation.ID, RequestDigest: result.Reservation.RequestDigest}))
		_, err = repo.BeginTaskDispatch(ctx, result.Claim.Token(), id.NewAgentRunID())
		require.ErrorIs(t, err, execution.ErrBudgetExceeded)
		_, err = repo.RenewTaskClaim(ctx, result.Claim.Token(), 2*time.Minute)
		require.ErrorIs(t, err, execution.ErrBudgetExceeded)
		retry, err := repo.AcquireBudgetedTaskClaim(ctx, req)
		require.NoError(t, err)
		require.False(t, retry.Fresh)
		require.Nil(t, retry.Reservation.Settlement, "retry retains the acquisition, not a new spend allowance")
	})
	t.Run("overrun_fences_dispatch", func(t *testing.T) {
		ctx, req := budgetedClaimFixture(t, backend)
		repo := assurance.New(driver)
		result, err := repo.AcquireBudgetedTaskClaim(ctx, req)
		require.NoError(t, err)
		other := req.Reservation
		other.ID = id.New(execution.PrefixReservation)
		other.Allocation.Tokens = 100
		retained, err := repo.Reserve(ctx, other)
		require.NoError(t, err)
		require.NoError(t, repo.Settle(ctx, execution.Settlement{ReservationID: other.ID, RequestDigest: retained.RequestDigest, Usage: execution.ReservationUsage{Known: execution.KnownTokens, Actual: execution.Resources{Tokens: 1500}}, Receipt: "overrun"}))
		_, err = repo.BeginTaskDispatch(ctx, result.Claim.Token(), id.NewAgentRunID())
		require.ErrorIs(t, err, execution.ErrBudgetExceeded)
	})
	t.Run("ancestor_overrun_fences_dispatch", func(t *testing.T) {
		ctx, req := budgetedClaimFixture(t, backend)
		repo := assurance.New(driver)
		parent := req.Reservation
		parent.ID = id.New(execution.PrefixReservation)
		parent.Kind = execution.ReservationBranch
		parent.Allocation = execution.Resources{Steps: 10, Tokens: 600, ToolCalls: 10, FanOut: 4, USDMicros: 10000}
		_, err := repo.Reserve(ctx, parent)
		require.NoError(t, err)
		req.Reservation.ParentID = parent.ID
		req.Reservation.Allocation.Tokens = 300
		result, err := repo.AcquireBudgetedTaskClaim(ctx, req)
		require.NoError(t, err)
		other := req.Reservation
		other.ID = id.New(execution.PrefixReservation)
		other.Allocation.Tokens = 100
		retained, err := repo.Reserve(ctx, other)
		require.NoError(t, err)
		require.NoError(t, repo.Settle(ctx, execution.Settlement{ReservationID: other.ID, RequestDigest: retained.RequestDigest, Usage: execution.ReservationUsage{Known: execution.KnownTokens, Actual: execution.Resources{Tokens: 400}}, Receipt: "branch-overrun"}))
		snapshot, err := repo.GetBudgetSnapshot(ctx, req.Claim.PlanID)
		require.NoError(t, err)
		require.True(t, snapshot.Policy.Limits.Covers(snapshot.Charged), "root still has capacity")
		_, err = repo.BeginTaskDispatch(ctx, result.Claim.Token(), id.NewAgentRunID())
		require.ErrorIs(t, err, execution.ErrBudgetExceeded, "ancestor allowance must also hold")
	})
	t.Run("renewal_and_dispatch", func(t *testing.T) {
		ctx, req := budgetedClaimFixture(t, backend)
		repo := assurance.New(driver)
		result, err := repo.AcquireBudgetedTaskClaim(ctx, req)
		require.NoError(t, err)
		renewed, err := repo.RenewTaskClaim(ctx, result.Claim.Token(), 2*time.Minute)
		require.NoError(t, err)
		child := id.NewAgentRunID()
		dispatch, err := repo.BeginTaskDispatch(ctx, renewed.Token(), child)
		require.NoError(t, err)
		require.True(t, dispatch.Fresh)
		require.NoError(t, repo.Settle(ctx, execution.Settlement{ReservationID: req.Reservation.ID, RequestDigest: result.Reservation.RequestDigest}))
		retry, err := repo.BeginTaskDispatch(ctx, renewed.Token(), child)
		require.NoError(t, err)
		require.False(t, retry.Fresh)
		require.Equal(t, dispatch.Claim, retry.Claim)
	})
	t.Run("reservation_deadline_bounds_lease", func(t *testing.T) {
		ctx, req := budgetedClaimFixture(t, backend)
		clock := &jobClock{at: time.Now().UTC()}
		repo := assurance.New(driver).WithClock(clock)
		req.Reservation.Deadline = clock.Now().Add(30 * time.Second)
		_, err := repo.AcquireBudgetedTaskClaim(ctx, req)
		require.ErrorIs(t, err, execution.ErrBudgetExceeded)
		req.Claim.Duration = 10 * time.Second
		result, err := repo.AcquireBudgetedTaskClaim(ctx, req)
		require.NoError(t, err)
		_, err = repo.RenewTaskClaim(ctx, result.Claim.Token(), time.Minute)
		require.ErrorIs(t, err, execution.ErrBudgetExceeded)
		clock.advance(30 * time.Second)
		released, err := repo.ExpireTaskClaim(ctx, req.Claim.PlanID, req.Claim.TaskID, result.Claim.Revision)
		require.NoError(t, err)
		require.Equal(t, coordination.ClaimReleased, released.State)
		snapshot, err := repo.GetBudgetSnapshot(ctx, req.Claim.PlanID)
		require.NoError(t, err)
		require.EqualValues(t, 600, snapshot.Charged.Tokens, "claim expiry cannot refund unknown usage")
	})
	t.Run("replan_and_scope", func(t *testing.T) {
		ctx, req := budgetedClaimFixture(t, backend)
		repo := assurance.New(driver)
		for _, other := range []context.Context{context.Background(), cortex.WithScope(ctx, cortex.Scope{Levels: req.Reservation.Scope.Levels[:1]}), ctxWithScope("another")} {
			_, err := repo.AcquireBudgetedTaskClaim(other, req)
			require.Error(t, err)
		}
		result, err := repo.AcquireBudgetedTaskClaim(ctx, req)
		require.NoError(t, err)
		p, err := repo.GetPlan(ctx, req.Claim.PlanID)
		require.NoError(t, err)
		p.Tasks[0].Instruction = "revised task"
		_, err = repo.CommitRevision(ctx, planning.Revision{Plan: p, Actor: "host", Reason: "replan"}, p.Revision)
		require.NoError(t, err)
		_, err = repo.BeginTaskDispatch(ctx, result.Claim.Token(), id.NewAgentRunID())
		require.ErrorIs(t, err, coordination.ErrClaimConflict)
		retry, err := repo.AcquireBudgetedTaskClaim(ctx, req)
		require.NoError(t, err)
		require.False(t, retry.Fresh)
	})
	t.Run("expiry_after_binding_rolls_back", func(t *testing.T) {
		ctx, req := budgetedClaimFixture(t, backend)
		clock := &jobClock{at: time.Now().UTC()}
		faulty := assurance.New(&claimWriteDriver{Driver: driver, plans: plans, clock: clock, advanceKind: "task_claim_budget"}).WithClock(clock)
		_, err := faulty.AcquireBudgetedTaskClaim(ctx, req)
		require.ErrorIs(t, err, coordination.ErrClaimConflict)
		repo := assurance.New(driver)
		_, err = repo.GetTaskClaim(ctx, req.Claim.PlanID, req.Claim.TaskID)
		require.ErrorIs(t, err, coordination.ErrClaimNotFound)
		snapshot, err := repo.GetBudgetSnapshot(ctx, req.Claim.PlanID)
		require.NoError(t, err)
		require.Zero(t, snapshot.Reservations)
	})
	t.Run("backward_clock_between_claim_and_reservation", func(t *testing.T) {
		ctx, req := budgetedClaimFixture(t, backend)
		clock := &claimBudgetClock{at: time.Now().UTC()}
		result, err := assurance.New(driver).WithClock(clock).AcquireBudgetedTaskClaim(ctx, req)
		require.ErrorIs(t, err, coordination.ErrClaimConflict)
		require.Zero(t, result)
		_, err = assurance.New(driver).GetTaskClaim(ctx, req.Claim.PlanID, req.Claim.TaskID)
		require.ErrorIs(t, err, coordination.ErrClaimNotFound)
	})
	t.Run("different_tasks_share_capacity", func(t *testing.T) {
		ctx, req := budgetedClaimFixture(t, backend)
		repo := assurance.New(driver)
		p, err := repo.GetPlan(ctx, req.Claim.PlanID)
		require.NoError(t, err)
		second := p.Tasks[0]
		second.ID = id.New(planning.PrefixTask)
		second.Output.Name = "second-report"
		p.Tasks = append(p.Tasks, second)
		p, err = repo.CommitRevision(ctx, planning.Revision{Plan: p, Actor: "host", Reason: "parallel tasks"}, p.Revision)
		require.NoError(t, err)
		req.Claim.PlanRevision, req.Claim.PlanDigest, req.Reservation.PlanDigest = p.Revision, p.Digest, p.Digest
		other := req
		other.Claim.ID = id.New(coordination.PrefixClaim)
		other.Claim.TaskID, other.Reservation.TaskID = second.ID, second.ID
		other.Reservation.ID = id.New(execution.PrefixReservation)
		other.Reservation.OwnerRef = other.Claim.ID.String()
		results := make(chan error, 2)
		for _, request := range []coordination.BudgetedClaimRequest{req, other} {
			go func() { _, claimErr := repo.AcquireBudgetedTaskClaim(ctx, request); results <- claimErr }()
		}
		wins := 0
		for range 2 {
			if claimErr := <-results; claimErr == nil {
				wins++
			} else {
				require.ErrorIs(t, claimErr, execution.ErrBudgetExceeded)
			}
		}
		require.Equal(t, 1, wins)
		claims, err := repo.ListTaskClaims(ctx, coordination.ClaimListRequest{PlanID: p.ID, Limit: 10})
		require.NoError(t, err)
		require.Len(t, claims, 1)
		snapshot, err := repo.GetBudgetSnapshot(ctx, p.ID)
		require.NoError(t, err)
		require.Equal(t, 1, snapshot.Reservations)
	})
	t.Run("concurrent_acquisition", func(t *testing.T) {
		ctx, req := budgetedClaimFixture(t, backend)
		repo := assurance.New(driver)
		results := make(chan error, 8)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			request := req
			request.Claim.ID = id.New(coordination.PrefixClaim)
			request.Reservation.ID = id.New(execution.PrefixReservation)
			request.Reservation.OwnerRef = request.Claim.ID.String()
			wg.Go(func() { _, err := repo.AcquireBudgetedTaskClaim(ctx, request); results <- err })
		}
		wg.Wait()
		close(results)
		wins := 0
		for err := range results {
			if err == nil {
				wins++
			} else {
				require.ErrorIs(t, err, coordination.ErrClaimConflict)
			}
		}
		require.Equal(t, 1, wins)
		snapshot, err := repo.GetBudgetSnapshot(ctx, req.Claim.PlanID)
		require.NoError(t, err)
		require.Equal(t, 1, snapshot.Reservations)
	})
}

// claimBudgetClock moves backward after the claim's final time check.
type claimBudgetClock struct {
	mu    sync.Mutex
	at    time.Time
	reads int
}

func (c *claimBudgetClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads++
	if c.reads > 2 {
		return c.at.Add(-time.Second)
	}
	return c.at
}
