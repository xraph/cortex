package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/store"
)

func orchestrationBudgetFixture(t *testing.T, backend store.Store) (context.Context, orchestration.BudgetStore, orchestration.BudgetPolicy, execution.ReservationRequest) {
	t.Helper()
	s := resultCapability[orchestration.BudgetStore](t, backend)
	ctx := ctxWithScope("orchestration-budget")
	allocation := execution.Resources{Steps: 2, Tokens: 600, ToolCalls: 2, FanOut: 1}
	p := orchestration.BudgetPolicy{SchemaVersion: 1, ExecutionID: id.NewOrchestrationID(), PreparedDigest: evidence.Digest([]byte("prepared-team")), Scope: cortex.ScopeFromContext(ctx), Limits: execution.Resources{Steps: 10, Tokens: 1000, ToolCalls: 10, FanOut: 8}, Participants: map[string]execution.Resources{"worker": allocation}, ParticipantDigests: map[string]string{"worker": evidence.Digest([]byte("worker-v1"))}, Deadline: time.Now().Add(time.Hour), TokenOnly: true}
	r := execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.ExecutionID, PlanDigest: p.PreparedDigest, Scope: p.Scope, TaskID: id.NewAgentRunID(), OwnerRef: "worker", Kind: execution.ReservationLeaf, Allocation: allocation, Deadline: p.Deadline}
	require.NoError(t, s.CreateOrchestrationBudget(ctx, p))
	return ctx, s, p, r
}

func testOrchestrationBudgetConcurrent(t *testing.T, newStore func(*testing.T) store.Store) {
	ctx, s, p, r := orchestrationBudgetFixture(t, newStore(t))
	results := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		request := r
		request.ID, request.TaskID = id.New(execution.PrefixReservation), id.NewAgentRunID()
		wg.Go(func() { _, err := s.ReserveOrchestrationChild(ctx, request); results <- err })
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
	snapshot, err := s.GetOrchestrationBudgetSnapshot(ctx, p.ExecutionID)
	require.NoError(t, err)
	require.EqualValues(t, 600, snapshot.Charged.Tokens)
	require.Equal(t, 1, snapshot.Reservations)
}

func testOrchestrationBudgetLifecycle(t *testing.T, newStore func(*testing.T) store.Store) {
	ctx, s, p, r := orchestrationBudgetFixture(t, newStore(t))
	first, err := s.ReserveOrchestrationChild(ctx, r)
	require.NoError(t, err)
	again, err := s.ReserveOrchestrationChild(ctx, r)
	require.NoError(t, err)
	require.Equal(t, first, again)
	require.NoError(t, s.CreateOrchestrationBudget(ctx, p))
	changed := p
	changed.Limits.Tokens++
	require.ErrorIs(t, s.CreateOrchestrationBudget(ctx, changed), execution.ErrReservationConflict)
	duplicate := r
	duplicate.ID = id.New(execution.PrefixReservation)
	_, err = s.ReserveOrchestrationChild(ctx, duplicate)
	require.ErrorIs(t, err, execution.ErrReservationConflict)
	settlement := execution.Settlement{ReservationID: r.ID, RequestDigest: first.RequestDigest, Usage: execution.ReservationUsage{Actual: execution.Resources{Tokens: 100}, Known: execution.KnownTokens}, Receipt: "host:usage"}
	require.NoError(t, s.SettleOrchestrationChild(ctx, settlement))
	require.NoError(t, s.SettleOrchestrationChild(ctx, settlement))
	conflict := settlement
	conflict.Usage.Actual.Tokens++
	require.ErrorIs(t, s.SettleOrchestrationChild(ctx, conflict), execution.ErrReservationConflict)
	r.ID, r.TaskID = id.New(execution.PrefixReservation), id.NewAgentRunID()
	second, err := s.ReserveOrchestrationChild(ctx, r)
	require.NoError(t, err)
	require.NoError(t, s.CloseOrchestrationBudget(ctx, p.ExecutionID))
	require.NoError(t, s.CloseOrchestrationBudget(ctx, p.ExecutionID))
	require.NoError(t, s.CreateOrchestrationBudget(ctx, p), "same policy must not reopen admission")
	again, err = s.ReserveOrchestrationChild(ctx, r)
	require.NoError(t, err)
	require.Equal(t, second, again)
	newRequest := r
	newRequest.ID, newRequest.TaskID = id.New(execution.PrefixReservation), id.NewAgentRunID()
	_, err = s.ReserveOrchestrationChild(ctx, newRequest)
	require.ErrorIs(t, err, execution.ErrBudgetExceeded)
	require.NoError(t, s.SettleOrchestrationChild(ctx, execution.Settlement{ReservationID: r.ID, RequestDigest: second.RequestDigest}))
	snapshot, err := s.GetOrchestrationBudgetSnapshot(ctx, p.ExecutionID)
	require.NoError(t, err)
	require.True(t, snapshot.Closed)
	require.Equal(t, p.ParticipantDigests, snapshot.Policy.ParticipantDigests)
	require.EqualValues(t, 700, snapshot.Charged.Tokens)
	list, err := s.ListOrchestrationReservations(ctx, execution.ReservationListRequest{PlanID: p.ExecutionID, Limit: 1})
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.NotNil(t, list[0].Settlement)
	next, err := s.ListOrchestrationReservations(ctx, execution.ReservationListRequest{PlanID: p.ExecutionID, After: list[0].Request.ID, Limit: 1})
	require.NoError(t, err)
	require.Len(t, next, 1)
	_, err = s.GetOrchestrationBudget(context.Background(), p.ExecutionID)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	_, err = s.GetOrchestrationBudget(ctxWithScope("other"), p.ExecutionID)
	require.ErrorIs(t, err, execution.ErrReservationNotFound)
	_, err = s.GetOrchestrationBudgetSnapshot(ctxWithScope("other"), p.ExecutionID)
	require.ErrorIs(t, err, execution.ErrReservationNotFound)
	require.ErrorIs(t, s.CloseOrchestrationBudget(ctxWithScope("other"), p.ExecutionID), execution.ErrReservationNotFound)
	require.ErrorIs(t, s.SettleOrchestrationChild(ctxWithScope("other"), settlement), execution.ErrReservationNotFound)
}

func testOrchestrationBudgetDenials(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx, s, p, request := orchestrationBudgetFixture(t, backend)
	for _, mode := range []string{"participant", "digest", "run", "parent", "allocation", "scope", "account"} {
		t.Run(mode, func(t *testing.T) {
			r := request
			switch mode {
			case "participant":
				r.OwnerRef = "outsider"
			case "digest":
				r.PlanDigest = evidence.Digest([]byte("changed"))
			case "run":
				r.TaskID = id.New("ptask")
			case "parent":
				r.ParentID = id.New(execution.PrefixReservation)
			case "allocation":
				r.Allocation.Tokens++
			case "scope":
				r.Scope = cortex.ScopeFromContext(ctxWithScope("other"))
			case "account":
				r.PlanID = id.New("plan")
			}
			_, err := s.ReserveOrchestrationChild(ctx, r)
			if mode == "scope" {
				require.ErrorIs(t, err, cortex.ErrInvalidScope)
			} else {
				require.ErrorIs(t, err, execution.ErrInvalidReservation)
			}
		})
	}
	snapshot, err := s.GetOrchestrationBudgetSnapshot(ctx, p.ExecutionID)
	require.NoError(t, err)
	require.Zero(t, snapshot.Reservations)
	reserved, err := s.ReserveOrchestrationChild(ctx, request)
	require.NoError(t, err)
	planStore := resultCapability[execution.BudgetStore](t, backend)
	_, err = planStore.Reserve(ctx, request)
	require.Error(t, err)
	settlement := execution.Settlement{ReservationID: request.ID, RequestDigest: reserved.RequestDigest, Usage: execution.ReservationUsage{Actual: execution.Resources{Tokens: 1200}, Known: execution.KnownTokens}, Receipt: "host:overrun"}
	require.Error(t, planStore.Settle(ctx, settlement), "plan entry point must not settle a team")
	unchanged, err := s.GetReservation(ctx, request.ID)
	require.NoError(t, err)
	require.Nil(t, unchanged.Settlement)
	require.NoError(t, s.SettleOrchestrationChild(ctx, settlement))
	snapshot, err = s.GetOrchestrationBudgetSnapshot(ctx, p.ExecutionID)
	require.NoError(t, err)
	require.EqualValues(t, 1200, snapshot.Charged.Tokens)
	request.ID, request.TaskID = id.New(execution.PrefixReservation), id.NewAgentRunID()
	_, err = s.ReserveOrchestrationChild(ctx, request)
	require.ErrorIs(t, err, execution.ErrBudgetExceeded)
	planCtx, _, planRequest := budgetFixture(t, backend)
	planReservation, err := planStore.Reserve(planCtx, planRequest)
	require.NoError(t, err)
	require.ErrorIs(t, s.SettleOrchestrationChild(planCtx, execution.Settlement{ReservationID: planRequest.ID, RequestDigest: planReservation.RequestDigest}), execution.ErrInvalidReservation)
	unchanged, err = planStore.GetReservation(planCtx, planRequest.ID)
	require.NoError(t, err)
	require.Nil(t, unchanged.Settlement)
}

func testOrchestrationBudgetClosureRace(t *testing.T, newStore func(*testing.T) store.Store) {
	ctx, s, p, request := orchestrationBudgetFixture(t, newStore(t))
	// Each admitted child consumes one slot. The close transaction serializes
	// with reserve, so every successful admission remains visible after closure.
	request.Allocation = execution.Resources{Tokens: 1, FanOut: 1}
	start := make(chan struct{})
	results := make(chan error, 9)
	var wg sync.WaitGroup
	for range 8 {
		r := request
		r.ID, r.TaskID = id.New(execution.PrefixReservation), id.NewAgentRunID()
		wg.Go(func() { <-start; _, err := s.ReserveOrchestrationChild(ctx, r); results <- err })
	}
	wg.Go(func() { <-start; results <- s.CloseOrchestrationBudget(ctx, p.ExecutionID) })
	close(start)
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, execution.ErrBudgetExceeded) {
			t.Fatal(err)
		}
	}
	snapshot, err := s.GetOrchestrationBudgetSnapshot(ctx, p.ExecutionID)
	require.NoError(t, err)
	require.True(t, snapshot.Closed)
	require.Equal(t, wins-1, snapshot.Reservations)
	require.EqualValues(t, wins-1, snapshot.Charged.Tokens)
	_, err = s.ReserveOrchestrationChild(ctx, request)
	require.ErrorIs(t, err, execution.ErrBudgetExceeded)
}
