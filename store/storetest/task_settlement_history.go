package storetest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

func testTaskSettlementHistory(t *testing.T, backend store.Store, driver assurance.Driver) {
	t.Helper()
	t.Run("discovery and exact scope", func(t *testing.T) {
		f, request := usageSettlementStoreFixture(t, backend, driver, true)
		accepted, err := f.repo.GetTaskClaim(f.ctx, request.Usage.Claim.PlanID, request.Usage.Claim.TaskID)
		require.NoError(t, err)
		supersedeAcceptedTask(t, f, accepted)
		page := coordination.TaskAcceptanceListRequest{PlanID: accepted.PlanID, Limit: 1}
		claims, err := f.repo.ListAcceptedTaskClaims(f.ctx, page)
		require.NoError(t, err)
		require.Equal(t, []coordination.Claim{accepted}, claims)
		page.After = *accepted.AcceptanceID
		claims, err = f.repo.ListAcceptedTaskClaims(f.ctx, page)
		require.NoError(t, err)
		require.Empty(t, claims)
		page = coordination.TaskAcceptanceListRequest{PlanID: accepted.PlanID, Limit: 1}
		claims, err = f.repo.ListAcceptedTaskClaims(ctxWithScope("other"), page)
		require.NoError(t, err)
		require.Empty(t, claims)
		_, err = f.repo.ListAcceptedTaskClaims(context.Background(), page)
		require.ErrorIs(t, err, cortex.ErrNoScope)
		page.Limit = 257
		_, err = f.repo.ListAcceptedTaskClaims(f.ctx, page)
		require.ErrorIs(t, err, coordination.ErrInvalidTaskAcceptance)
	})
	for _, missing := range []string{"task_claim_revision", "task_acceptance", "task_dispatch"} {
		t.Run("missing "+missing, func(t *testing.T) {
			f, request := usageSettlementStoreFixture(t, backend, driver, true)
			claim, err := f.repo.GetTaskClaim(f.ctx, request.Usage.Claim.PlanID, request.Usage.Claim.TaskID)
			require.NoError(t, err)
			supersedeAcceptedTask(t, f, claim)
			plans, ok := driver.(assurance.PlanDriver)
			require.True(t, ok)
			fault := &submissionFenceDriver{Driver: driver, plans: plans}
			fault.afterSelect = func(_ context.Context, q assurance.Query, rows []assurance.Record) ([]assurance.Record, error) {
				if q.Kind == missing {
					return nil, nil
				}
				return rows, nil
			}
			receipt, err := assurance.New(fault).WithClock(f.clock).SettleTaskUsage(f.ctx, request)
			require.Error(t, err)
			require.Empty(t, receipt)
			allocation, err := f.repo.GetReservation(f.ctx, request.ReservationID)
			require.NoError(t, err)
			require.Nil(t, allocation.Settlement)
		})
	}
}
