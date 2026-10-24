package storetest

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/store"
)

func testReplanAccountSurvivesConcurrencyAndRevision(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	budgets := resultCapability[execution.BudgetStore](t, backend)
	plans := resultCapability[planning.Store](t, backend)
	ctx, p, h := assignmentFixture(t, backend)
	policy := execution.ReservationPolicy{SchemaVersion: 1, PlanID: p.ID, Scope: p.Scope, Limits: execution.Resources{Steps: 10, Tokens: 1000, ToolCalls: 10, FanOut: 10, Replans: 2}, Deadline: p.Budget.Deadline, TokenOnly: true}
	require.NoError(t, budgets.CreateReservationPolicy(ctx, policy))
	now := time.Now().UTC()
	progress := planning.ProgressSnapshot{SchemaVersion: 1, Plan: p, At: now, StartedAt: now.Add(-time.Minute), LastProgressAt: now.Add(-time.Minute), Attempts: []planning.ProgressAttempt{
		{PlanDigest: p.Digest, RunID: id.NewAgentRunID(), TaskID: h.TaskID, FinishedAt: now.Add(-2 * time.Second), FailureDigest: evidence.Digest([]byte("failure"))},
		{PlanDigest: p.Digest, RunID: id.NewAgentRunID(), TaskID: h.TaskID, FinishedAt: now.Add(-time.Second), FailureDigest: evidence.Digest([]byte("failure"))},
	}}
	var err error
	progress.Account, err = budgets.GetBudgetSnapshot(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, planning.ReplanPropose, planning.ProposeReplan(progress, planning.ReplanPolicy{}).Action)
	request := execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.ID, PlanDigest: p.Digest, Scope: p.Scope, TaskID: h.TaskID, OwnerRef: "replan-attempt", Kind: execution.ReservationLeaf, Allocation: execution.Resources{Steps: 1, Tokens: 10, FanOut: 1, Replans: 1}, Deadline: p.Budget.Deadline}
	type result struct {
		reservation execution.Reservation
		err         error
	}
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for range 8 {
		next := request
		next.ID = id.New(execution.PrefixReservation)
		wg.Go(func() { reserved, reserveErr := budgets.Reserve(ctx, next); results <- result{reserved, reserveErr} })
	}
	wg.Wait()
	close(results)
	var admitted []execution.Reservation
	for got := range results {
		if got.err == nil {
			admitted = append(admitted, got.reservation)
		} else {
			require.ErrorIs(t, got.err, execution.ErrBudgetExceeded)
		}
	}
	require.Len(t, admitted, 2)
	for _, reserved := range admitted {
		// Missing replan usage cannot refund an admitted attempt.
		require.NoError(t, budgets.Settle(ctx, execution.Settlement{ReservationID: reserved.Request.ID, RequestDigest: reserved.RequestDigest, Usage: execution.ReservationUsage{Known: execution.KnownTokens}, Receipt: "observed-token-usage"}))
	}
	progress.Account, err = budgets.GetBudgetSnapshot(ctx, p.ID)
	require.NoError(t, err)
	require.EqualValues(t, 2, progress.Account.Charged.Replans)
	require.Equal(t, planning.ReplanLimit, planning.ProposeReplan(progress, planning.ReplanPolicy{}).Reason)
	p.Tasks[0].Instruction = "use a different approach within the same contract"
	next, err := plans.CommitRevision(ctx, planning.Revision{Plan: p, Actor: "host", Reason: "reviewed plan revision"}, p.Revision)
	require.NoError(t, err)
	progress.Plan = next
	require.Equal(t, planning.ReplanInvalidSnapshot, planning.ProposeReplan(progress, planning.ReplanPolicy{}).Reason)
	progress.Attempts = nil
	progress.Account, err = budgets.GetBudgetSnapshot(ctx, next.ID)
	require.NoError(t, err)
	require.Equal(t, planning.ReplanLimit, planning.ProposeReplan(progress, planning.ReplanPolicy{MaxIdle: 30 * time.Second}).Reason)
	request.PlanDigest = next.Digest
	_, err = budgets.Reserve(ctx, request)
	require.ErrorIs(t, err, execution.ErrBudgetExceeded)
}
