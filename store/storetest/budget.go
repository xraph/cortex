package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

func budgetFixture(t *testing.T, backend store.Store) (context.Context, execution.ReservationPolicy, execution.ReservationRequest) {
	t.Helper()
	ctx, p, h := assignmentFixture(t, backend)
	policy := execution.ReservationPolicy{SchemaVersion: 1, PlanID: p.ID, Scope: p.Scope, Limits: execution.Resources{Steps: 10, Tokens: 1000, ToolCalls: 10, FanOut: 10, USDMicros: 10000}, Deadline: p.Budget.Deadline, PricingRevision: "price-1"}
	r := execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.ID, PlanDigest: p.Digest, Scope: p.Scope, TaskID: h.TaskID, OwnerRef: "claim/fence", Kind: execution.ReservationLeaf, Allocation: execution.Resources{Steps: 1, Tokens: 600, ToolCalls: 1, FanOut: 1, USDMicros: 1000}, Deadline: policy.Deadline, PricingRevision: policy.PricingRevision, MoneyBound: true}
	require.NoError(t, resultCapability[execution.BudgetStore](t, backend).CreateReservationPolicy(ctx, policy))
	return ctx, policy, r
}
func testConcurrentChildrenCannotOverspend(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	s := resultCapability[execution.BudgetStore](t, backend)
	ctx, _, r := budgetFixture(t, backend)
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		request := r
		request.ID = id.New(execution.PrefixReservation)
		wg.Go(func() { _, err := s.Reserve(ctx, request); results <- err })
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else {
			require.ErrorIs(t, err, execution.ErrBudgetExceeded)
		}
	}
	require.Equal(t, 1, wins)
	snapshot, err := s.GetBudgetSnapshot(ctx, r.PlanID)
	require.NoError(t, err)
	require.EqualValues(t, 600, snapshot.Charged.Tokens)
}
func testBudgetSettlements(t *testing.T, newStore func(*testing.T) store.Store) {
	t.Run("partial usage", func(t *testing.T) { testBudgetPartialUsage(t, newStore) })
	backend := newStore(t)
	s := resultCapability[execution.BudgetStore](t, backend)
	ctx, policy, r := budgetFixture(t, backend)
	reserved, err := s.Reserve(ctx, r)
	require.NoError(t, err)
	retry, err := s.Reserve(ctx, r)
	require.NoError(t, err)
	require.Equal(t, reserved, retry)
	require.NoError(t, s.CreateReservationPolicy(ctx, policy))
	changed := policy
	changed.Limits.Tokens++
	require.ErrorIs(t, s.CreateReservationPolicy(ctx, changed), execution.ErrReservationConflict)
	settlement := execution.Settlement{ReservationID: r.ID, RequestDigest: reserved.RequestDigest, Usage: execution.ReservationUsage{Actual: execution.Resources{Tokens: 100}, Known: execution.KnownTokens}, Receipt: "receipt-1"}
	require.NoError(t, s.Settle(ctx, settlement))
	require.NoError(t, s.Settle(ctx, settlement))
	changedSettlement := settlement
	changedSettlement.Usage.Actual.Tokens++
	require.ErrorIs(t, s.Settle(ctx, changedSettlement), execution.ErrReservationConflict)
	snapshot, err := s.GetBudgetSnapshot(ctx, r.PlanID)
	require.NoError(t, err)
	require.EqualValues(t, 100, snapshot.Charged.Tokens)
	require.EqualValues(t, 1000, snapshot.Charged.USDMicros)
	list, err := s.ListReservations(ctx, execution.ReservationListRequest{PlanID: r.PlanID, Limit: 1})
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.NotNil(t, list[0].Settlement)
	_, err = s.GetReservation(context.Background(), r.ID)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	_, err = s.GetReservation(ctxWithScope("other"), r.ID)
	require.ErrorIs(t, err, execution.ErrReservationNotFound)
	r.ID = id.New(execution.PrefixReservation)
	second, err := s.Reserve(ctx, r)
	require.NoError(t, err)
	require.NoError(t, s.Settle(ctx, execution.Settlement{ReservationID: r.ID, RequestDigest: second.RequestDigest}))
	snapshot, err = s.GetBudgetSnapshot(ctx, r.PlanID)
	require.NoError(t, err)
	require.EqualValues(t, 700, snapshot.Charged.Tokens, "unknown usage remains charged")
}
func testBudgetTree(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	s := resultCapability[execution.BudgetStore](t, backend)
	ctx, _, branch := budgetFixture(t, backend)
	branch.Kind = execution.ReservationBranch
	branch.Allocation = execution.Resources{Steps: 5, Tokens: 600, ToolCalls: 5, FanOut: 5, USDMicros: 5000}
	parent, err := s.Reserve(ctx, branch)
	require.NoError(t, err)
	child := branch
	child.ID = id.New(execution.PrefixReservation)
	child.ParentID = branch.ID
	child.Kind = execution.ReservationLeaf
	child.Allocation = execution.Resources{Steps: 1, Tokens: 400, ToolCalls: 1, FanOut: 1, USDMicros: 1000}
	leaf, err := s.Reserve(ctx, child)
	require.NoError(t, err)
	snapshot, err := s.GetBudgetSnapshot(ctx, branch.PlanID)
	require.NoError(t, err)
	require.Equal(t, branch.Allocation, snapshot.Charged)
	closeParent := execution.Settlement{ReservationID: branch.ID, RequestDigest: parent.RequestDigest, CloseBranch: true}
	require.ErrorIs(t, s.Settle(ctx, closeParent), execution.ErrReservationConflict)
	require.NoError(t, s.Settle(ctx, execution.Settlement{ReservationID: child.ID, RequestDigest: leaf.RequestDigest, Usage: execution.ReservationUsage{Actual: execution.Resources{Tokens: 900}, Known: execution.KnownTokens}, Receipt: "overrun"}))
	snapshot, err = s.GetBudgetSnapshot(ctx, branch.PlanID)
	require.NoError(t, err)
	require.EqualValues(t, 900, snapshot.Charged.Tokens, "overrun reaches the root before branch closes")
	extra := child
	extra.ID = id.New(execution.PrefixReservation)
	extra.ParentID = id.ID{}
	extra.Allocation.Tokens = 200
	_, err = s.Reserve(ctx, extra)
	require.ErrorIs(t, err, execution.ErrBudgetExceeded)
	require.NoError(t, s.Settle(ctx, closeParent))
	extra.ParentID = branch.ID
	extra.Allocation.Tokens = 1
	_, err = s.Reserve(ctx, extra)
	require.ErrorIs(t, err, execution.ErrReservationConflict)
	snapshot, err = s.GetBudgetSnapshot(ctx, branch.PlanID)
	require.NoError(t, err)
	require.EqualValues(t, 2, snapshot.Charged.FanOut)
}

func testBudgetDenials(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	s := resultCapability[execution.BudgetStore](t, backend)
	for _, mode := range []string{"scope", "plan digest", "task", "price", "money bound", "deadline", "parent", "negative", "fanout"} {
		t.Run(mode, func(t *testing.T) {
			ctx, _, r := budgetFixture(t, backend)
			switch mode {
			case "scope":
				ctx = ctxWithScope("other")
			case "plan digest":
				r.PlanDigest = evidence.Digest([]byte("other"))
			case "task":
				r.TaskID = id.New("ptask")
			case "price":
				r.PricingRevision = "other-price"
			case "money bound":
				r.MoneyBound = false
				r.PricingRevision = ""
				r.Allocation.USDMicros = 0
			case "deadline":
				r.Deadline = r.Deadline.Add(time.Second)
			case "parent":
				r.ParentID = id.New(execution.PrefixReservation)
			case "negative":
				r.Allocation.Tokens = -1
			case "fanout":
				r.Allocation.FanOut = 2
			}
			_, err := s.Reserve(ctx, r)
			require.Error(t, err)
		})
	}
}
