package storetest

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

func assignmentFixture(t *testing.T, backend store.Store, configure ...func(*planning.Plan)) (context.Context, planning.Plan, coordination.Handoff) {
	t.Helper()
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: id.New(coordination.PrefixAssignment).String()}, {Key: "project", Value: "one"}}}
	ctx := cortex.WithScope(context.Background(), scope)
	p := storedPlan(t, scope)
	for _, change := range configure {
		change(&p)
		var sealErr error
		p.Contract, sealErr = verification.SealContract(p.Contract)
		require.NoError(t, sealErr)
		p, sealErr = planning.Seal(p, planning.Limits{})
		require.NoError(t, sealErr)
	}
	plans, ok := backend.(planning.Store)
	require.True(t, ok)
	require.NoError(t, plans.CreatePlan(ctx, p))
	authority, err := planning.TaskAuthority(p, p.Tasks[0].ID)
	require.NoError(t, err)
	h, err := coordination.SealHandoff(coordination.Handoff{SchemaVersion: 1, ID: id.New(coordination.PrefixHandoff), AssignmentID: id.New(coordination.PrefixAssignment), Fence: 1, Scope: scope, PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: p.Tasks[0].ID, ParentRunID: id.NewAgentRunID(), Sender: coordination.Identity{Agent: "planner", Principal: "host"}, Receiver: coordination.Identity{Agent: "worker", Principal: "worker-1"}, Purpose: coordination.HandoffWork, GrantRef: "grant-1", Authority: authority, ContractDigest: p.Contract.Digest, Criteria: p.Tasks[0].Criteria, Deadline: authority.Budget.Deadline})
	require.NoError(t, err)
	return ctx, p, h
}
func assignmentChange(a coordination.Assignment, state coordination.AssignmentState) coordination.AssignmentChange {
	return coordination.AssignmentChange{AssignmentID: a.Handoff.AssignmentID, ExpectedRevision: a.Revision, HandoffDigest: a.Handoff.Digest, Fence: a.Handoff.Fence, State: state, Actor: a.Handoff.Receiver.Principal, Reason: "observed"}
}
func testAssignments(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	s, ok := backend.(coordination.AssignmentStore)
	require.True(t, ok, "assignment journal missing")
	ctx, p, h := assignmentFixture(t, backend)
	initial, err := s.ProposeAssignment(ctx, h)
	require.NoError(t, err)
	retry, err := s.ProposeAssignment(ctx, h)
	require.NoError(t, err)
	require.Equal(t, initial, retry)
	different := h
	different.Fence++
	different, err = coordination.SealHandoff(different)
	require.NoError(t, err)
	_, err = s.ProposeAssignment(ctx, different)
	require.ErrorIs(t, err, coordination.ErrAssignmentConflict)
	change := assignmentChange(initial, coordination.AssignmentAccepted)
	accepted, err := s.AdvanceAssignment(ctx, change)
	require.NoError(t, err)
	working, err := s.AdvanceAssignment(ctx, assignmentChange(accepted, coordination.AssignmentWorking))
	require.NoError(t, err)
	replay, err := s.AdvanceAssignment(ctx, change)
	require.NoError(t, err)
	require.Equal(t, accepted, replay, "historical retry must not append or return a different observation")
	change.Reason = "different"
	_, err = s.AdvanceAssignment(ctx, change)
	require.ErrorIs(t, err, coordination.ErrAssignmentConflict)
	current, err := s.GetAssignment(ctx, h.AssignmentID)
	require.NoError(t, err)
	require.Equal(t, working, current)
	current.Handoff.Criteria[0] = "caller mutation"
	current, err = s.GetAssignment(ctx, h.AssignmentID)
	require.NoError(t, err)
	require.Equal(t, working, current)
	history, err := s.AssignmentHistory(ctx, coordination.AssignmentHistoryRequest{AssignmentID: h.AssignmentID, Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []coordination.Assignment{initial, accepted}, history)
	history, err = s.AssignmentHistory(ctx, coordination.AssignmentHistoryRequest{AssignmentID: h.AssignmentID, AfterRevision: 2, Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []coordination.Assignment{working}, history)
	listed, err := s.ListAssignments(ctx, coordination.AssignmentListRequest{PlanID: p.ID, Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []coordination.Assignment{working}, listed)
	listed, err = s.ListAssignments(ctx, coordination.AssignmentListRequest{PlanID: p.ID, After: h.AssignmentID, Limit: 1})
	require.NoError(t, err)
	require.Empty(t, listed)
	for _, other := range []context.Context{context.Background(), cortex.WithScope(ctx, cortex.Scope{Levels: h.Scope.Levels[:1]}), cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})} {
		_, err = s.ProposeAssignment(other, h)
		require.Error(t, err)
		_, err = s.AdvanceAssignment(other, assignmentChange(working, coordination.AssignmentBlocked))
		require.Error(t, err)
		_, err = s.GetAssignment(other, h.AssignmentID)
		require.Error(t, err)
		_, err = s.AssignmentHistory(other, coordination.AssignmentHistoryRequest{AssignmentID: h.AssignmentID, Limit: 1})
		require.Error(t, err)
		listed, err = s.ListAssignments(other, coordination.AssignmentListRequest{PlanID: p.ID, Limit: 1})
		if other == context.Background() {
			require.ErrorIs(t, err, cortex.ErrNoScope)
		} else {
			require.NoError(t, err)
			require.Empty(t, listed)
		}
	}
	for _, limit := range []int{0, 257} {
		_, err = s.ListAssignments(ctx, coordination.AssignmentListRequest{PlanID: p.ID, Limit: limit})
		require.ErrorIs(t, err, coordination.ErrInvalidAssignment)
		_, err = s.AssignmentHistory(ctx, coordination.AssignmentHistoryRequest{AssignmentID: h.AssignmentID, Limit: limit})
		require.ErrorIs(t, err, coordination.ErrInvalidAssignment)
	}
	_, err = s.AssignmentHistory(ctx, coordination.AssignmentHistoryRequest{AssignmentID: h.AssignmentID, AfterRevision: -1, Limit: 1})
	require.ErrorIs(t, err, coordination.ErrInvalidAssignment)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.ProposeAssignment(cancelled, h)
	require.ErrorIs(t, err, context.Canceled)
	_, err = s.GetAssignment(cancelled, h.AssignmentID)
	require.ErrorIs(t, err, context.Canceled)
	_, err = s.AdvanceAssignment(cancelled, assignmentChange(working, coordination.AssignmentBlocked))
	require.ErrorIs(t, err, context.Canceled)
	_, err = s.AssignmentHistory(cancelled, coordination.AssignmentHistoryRequest{AssignmentID: h.AssignmentID, Limit: 1})
	require.ErrorIs(t, err, context.Canceled)
	_, err = s.ListAssignments(cancelled, coordination.AssignmentListRequest{PlanID: p.ID, Limit: 1})
	require.ErrorIs(t, err, context.Canceled)
}
func testAssignmentReplan(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	s := resultCapability[coordination.AssignmentStore](t, backend)
	plans := resultCapability[planning.Store](t, backend)
	ctx, p, h := assignmentFixture(t, backend)
	initial, err := s.ProposeAssignment(ctx, h)
	require.NoError(t, err)
	_, err = plans.CommitRevision(ctx, planning.Revision{Plan: p, Actor: "host", Reason: "replan"}, 1)
	require.NoError(t, err)
	_, err = s.AdvanceAssignment(ctx, assignmentChange(initial, coordination.AssignmentAccepted))
	require.ErrorIs(t, err, coordination.ErrInvalidHandoff)
	different := h
	different.AssignmentID = id.New(coordination.PrefixAssignment)
	different, err = coordination.SealHandoff(different)
	require.NoError(t, err)
	_, err = s.ProposeAssignment(ctx, different)
	require.ErrorIs(t, err, coordination.ErrInvalidHandoff)
	terminal, err := s.AdvanceAssignment(ctx, assignmentChange(initial, coordination.AssignmentCancelled))
	require.NoError(t, err, "a stale plan must still permit cancellation")
	require.Equal(t, coordination.AssignmentCancelled, terminal.State)
	replay, err := s.ProposeAssignment(ctx, h)
	require.NoError(t, err)
	require.Equal(t, initial, replay, "retry only acknowledges the original proposal")
	_, err = s.AdvanceAssignment(ctx, assignmentChange(terminal, coordination.AssignmentAccepted))
	require.ErrorIs(t, err, coordination.ErrAssignmentConflict)
	history, err := s.AssignmentHistory(ctx, coordination.AssignmentHistoryRequest{AssignmentID: h.AssignmentID, Limit: 10})
	require.NoError(t, err)
	require.Equal(t, []coordination.Assignment{initial, terminal}, history)
}
func testAssignmentConcurrent(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	s := resultCapability[coordination.AssignmentStore](t, backend)
	ctx, _, h := assignmentFixture(t, backend)
	const workers = 12
	results := make([]coordination.Assignment, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Go(func() { results[i], errs[i] = s.ProposeAssignment(ctx, h) })
	}
	wg.Wait()
	for i := range workers {
		require.NoError(t, errs[i])
		require.Equal(t, results[0], results[i])
	}
	initial := results[0]
	for i := range workers {
		wg.Go(func() {
			change := assignmentChange(initial, coordination.AssignmentAccepted)
			change.Reason = fmt.Sprintf("observation %d", i)
			results[i], errs[i] = s.AdvanceAssignment(ctx, change)
		})
	}
	wg.Wait()
	successes := 0
	for _, err := range errs {
		if err == nil {
			successes++
		} else {
			require.ErrorIs(t, err, coordination.ErrAssignmentConflict)
		}
	}
	require.Equal(t, 1, successes)
	history, err := s.AssignmentHistory(ctx, coordination.AssignmentHistoryRequest{AssignmentID: h.AssignmentID, Limit: 256})
	require.NoError(t, err)
	require.Len(t, history, 2)
	require.Equal(t, initial, history[0])
}
