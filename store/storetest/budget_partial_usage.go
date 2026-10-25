package storetest

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

func testBudgetPartialUsage(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, overflow := range []bool{false, true} {
		name := "overrun"
		if overflow {
			name = "overflow"
		}
		t.Run(name, func(t *testing.T) {
			backend := newStore(t)
			s := resultCapability[execution.BudgetStore](t, backend)
			ctx, _, request := budgetFixture(t, backend)
			reserved, err := s.Reserve(ctx, request)
			require.NoError(t, err)
			settlement := execution.Settlement{ReservationID: request.ID, RequestDigest: reserved.RequestDigest,
				Minimum: &execution.Resources{Tokens: 1100, USDMicros: 50}, Receipt: "authenticated-partial-usage"}
			if overflow {
				settlement.Minimum.Tokens = math.MaxInt64
				settlement.Overflow = execution.KnownTokens
			}
			require.NoError(t, s.Settle(ctx, settlement))
			require.NoError(t, s.Settle(ctx, settlement))
			stored, err := s.GetReservation(ctx, request.ID)
			require.NoError(t, err)
			require.NotNil(t, stored.Settlement)
			require.Equal(t, settlement, stored.Settlement.Request)
			list, err := s.ListReservations(ctx, execution.ReservationListRequest{PlanID: request.PlanID, Limit: 10})
			require.NoError(t, err)
			require.Len(t, list, 1)
			require.Equal(t, stored.Settlement, list[0].Settlement)
			snapshot, err := s.GetBudgetSnapshot(ctx, request.PlanID)
			require.NoError(t, err)
			require.Equal(t, settlement.Minimum.Tokens, snapshot.Charged.Tokens)
			require.EqualValues(t, 1000, snapshot.Charged.USDMicros)
			require.Equal(t, overflow, snapshot.Overflow)
			request.ID = id.New(execution.PrefixReservation)
			request.Allocation.Tokens = 0
			_, err = s.Reserve(ctx, request)
			require.ErrorIs(t, err, execution.ErrBudgetExceeded)
			settlement.Minimum.USDMicros++
			require.ErrorIs(t, s.Settle(ctx, settlement), execution.ErrReservationConflict)
			unchanged, err := s.GetReservation(ctx, stored.Request.ID)
			require.NoError(t, err)
			require.Equal(t, stored, unchanged)
		})
	}
}
