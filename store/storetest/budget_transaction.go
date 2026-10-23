package storetest

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

// BudgetTransactions exercises shared accounting over real plan transactions.
func BudgetTransactions(t *testing.T, backend store.Store, driver assurance.Driver) {
	plans, ok := driver.(assurance.PlanDriver)
	require.True(t, ok)
	t.Run("rollback", func(t *testing.T) {
		ctx, _, request := budgetFixture(t, backend)
		faulty := assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, failInsert: true})
		result, err := faulty.Reserve(ctx, request)
		require.ErrorIs(t, err, errAssignmentInjected)
		require.Zero(t, result)
		snapshot, err := assurance.New(driver).GetBudgetSnapshot(ctx, request.PlanID)
		require.NoError(t, err)
		require.Zero(t, snapshot.Reservations)
	})
	t.Run("lost_acknowledgments", func(t *testing.T) {
		ctx, _, request := budgetFixture(t, backend)
		faulty := assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, lostAck: true})
		result, err := faulty.Reserve(ctx, request)
		require.ErrorIs(t, err, errAssignmentInjected)
		require.Zero(t, result)
		repo := assurance.New(driver)
		retained, err := repo.Reserve(ctx, request)
		require.NoError(t, err)
		settled := execution.Settlement{ReservationID: request.ID, RequestDigest: retained.RequestDigest, Usage: execution.ReservationUsage{Known: execution.KnownTokens, Actual: execution.Resources{Tokens: 50}}, Receipt: "provider-50"}
		require.ErrorIs(t, faulty.Settle(ctx, settled), errAssignmentInjected)
		require.NoError(t, repo.Settle(ctx, settled))
		snapshot, err := repo.GetBudgetSnapshot(ctx, request.PlanID)
		require.NoError(t, err)
		require.Equal(t, 1, snapshot.Reservations)
		require.EqualValues(t, 50, snapshot.Charged.Tokens)
	})
	t.Run("deadline_includes_paused_time", func(t *testing.T) {
		ctx, policy, request := budgetFixture(t, backend)
		clock := &jobClock{at: time.Now().UTC()}
		repo := assurance.New(driver).WithClock(clock)
		retained, err := repo.Reserve(ctx, request)
		require.NoError(t, err)
		clock.advance(policy.Deadline.Sub(clock.Now()))
		another := request
		another.ID = id.New(execution.PrefixReservation)
		_, err = repo.Reserve(ctx, another)
		require.Error(t, err)
		require.NoError(t, repo.Settle(ctx, execution.Settlement{ReservationID: request.ID, RequestDigest: retained.RequestDigest}))
		snapshot, err := repo.GetBudgetSnapshot(ctx, request.PlanID)
		require.NoError(t, err)
		require.EqualValues(t, 600, snapshot.Charged.Tokens, "expiry cannot refund unknown consumption")
	})
	t.Run("replan_cannot_reset_account", func(t *testing.T) {
		ctx, _, request := budgetFixture(t, backend)
		repo := assurance.New(driver)
		retained, err := repo.Reserve(ctx, request)
		require.NoError(t, err)
		p, err := repo.GetPlan(ctx, request.PlanID)
		require.NoError(t, err)
		p.Budget.MaxTokens = 500
		p.Tasks[0].Budget.MaxTokens = 500
		next, err := repo.CommitRevision(ctx, planning.Revision{Plan: p, Actor: "host", Reason: "narrow remaining work"}, p.Revision)
		require.NoError(t, err)
		another := request
		another.ID = id.New(execution.PrefixReservation)
		another.PlanDigest = next.Digest
		another.Allocation.Tokens = 100
		_, err = repo.Reserve(ctx, another)
		require.ErrorIs(t, err, execution.ErrBudgetExceeded)
		require.NoError(t, repo.Settle(ctx, execution.Settlement{ReservationID: request.ID, RequestDigest: retained.RequestDigest, Usage: execution.ReservationUsage{Known: execution.KnownTokens, Actual: execution.Resources{Tokens: 300}}, Receipt: "known-300"}))
		_, err = repo.Reserve(ctx, another)
		require.NoError(t, err)
		snapshot, err := repo.GetBudgetSnapshot(ctx, request.PlanID)
		require.NoError(t, err)
		require.EqualValues(t, 500, snapshot.Policy.Limits.Tokens)
		require.EqualValues(t, 400, snapshot.Charged.Tokens)
	})
	t.Run("overflow_retains_receipts_and_blocks_admission", func(t *testing.T) {
		ctx, _, request := budgetFixture(t, backend)
		repo := assurance.New(driver)
		request.Allocation.Tokens = 400
		first, err := repo.Reserve(ctx, request)
		require.NoError(t, err)
		request.ID = id.New(execution.PrefixReservation)
		second, err := repo.Reserve(ctx, request)
		require.NoError(t, err)
		for _, reservation := range []execution.Reservation{first, second} {
			require.NoError(t, repo.Settle(ctx, execution.Settlement{ReservationID: reservation.Request.ID, RequestDigest: reservation.RequestDigest, Usage: execution.ReservationUsage{Known: execution.KnownTokens, Actual: execution.Resources{Tokens: math.MaxInt64}}, Receipt: "retained-overrun"}))
		}
		snapshot, err := repo.GetBudgetSnapshot(ctx, request.PlanID)
		require.NoError(t, err)
		require.True(t, snapshot.Overflow)
		require.EqualValues(t, math.MaxInt64, snapshot.Charged.Tokens)
		loaded, err := repo.GetReservation(ctx, first.Request.ID)
		require.NoError(t, err)
		require.EqualValues(t, math.MaxInt64, loaded.Settlement.Request.Usage.Actual.Tokens)
		request.ID = id.New(execution.PrefixReservation)
		request.Allocation.Tokens = 0
		_, err = repo.Reserve(ctx, request)
		require.ErrorIs(t, err, execution.ErrBudgetExceeded)
	})
}
