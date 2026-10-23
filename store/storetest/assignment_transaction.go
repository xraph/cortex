package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

var errAssignmentInjected = errors.New("injected assignment storage failure")

type assignmentFaultDriver struct {
	assurance.Driver
	plans       assurance.PlanDriver
	afterInsert bool
	afterCommit bool
	entered     chan struct{}
	release     chan struct{}
	once        sync.Once
}

func (d *assignmentFaultDriver) TransactPlan(ctx context.Context, identifier id.ID, callback func(context.Context, assurance.Driver) error) error {
	err := d.plans.TransactPlan(ctx, identifier, func(tc context.Context, tx assurance.Driver) error {
		return callback(tc, assignmentFaultTx{Driver: tx, owner: d})
	})
	if err == nil && d.afterCommit {
		return errAssignmentInjected
	}
	return err
}

type assignmentFaultTx struct {
	assurance.Driver
	owner *assignmentFaultDriver
}

func (d assignmentFaultTx) Insert(ctx context.Context, row assurance.Record) error {
	if err := d.Driver.Insert(ctx, row); err != nil {
		return err
	}
	if d.owner.entered != nil {
		d.owner.once.Do(func() { close(d.owner.entered) })
		select {
		case <-d.owner.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if d.owner.afterInsert {
		return errAssignmentInjected
	}
	return nil
}

// AssignmentTransactions exercises rollback, ambiguous acknowledgements and
// serialization with a separate plan writer against each actual database.
func AssignmentTransactions(t *testing.T, backend store.Store, driver assurance.Driver) {
	plans, ok := driver.(assurance.PlanDriver)
	require.True(t, ok)
	journal := resultCapability[coordination.AssignmentStore](t, backend)
	t.Run("missing transaction", func(t *testing.T) {
		ctx, p, h := assignmentFixture(t, backend)
		repo := assurance.New(struct{ assurance.Driver }{driver})
		_, err := repo.ProposeAssignment(ctx, h)
		require.ErrorIs(t, err, coordination.ErrAssignmentUnavailable)
		_, err = repo.CommitRevision(ctx, planning.Revision{Plan: p, Actor: "host", Reason: "replan"}, 1)
		require.ErrorIs(t, err, coordination.ErrAssignmentUnavailable)
		_, err = journal.GetAssignment(ctx, h.AssignmentID)
		require.ErrorIs(t, err, coordination.ErrAssignmentNotFound)
	})
	t.Run("rollback", func(t *testing.T) {
		ctx, _, h := assignmentFixture(t, backend)
		fault := assurance.New(&assignmentFaultDriver{Driver: driver, plans: plans, afterInsert: true})
		result, err := fault.ProposeAssignment(ctx, h)
		require.ErrorIs(t, err, errAssignmentInjected)
		require.Zero(t, result)
		_, err = journal.GetAssignment(ctx, h.AssignmentID)
		require.ErrorIs(t, err, coordination.ErrAssignmentNotFound)
		initial, err := journal.ProposeAssignment(ctx, h)
		require.NoError(t, err)
		result, err = fault.AdvanceAssignment(ctx, assignmentChange(initial, coordination.AssignmentAccepted))
		require.ErrorIs(t, err, errAssignmentInjected)
		require.Zero(t, result)
		history, err := journal.AssignmentHistory(ctx, coordination.AssignmentHistoryRequest{AssignmentID: h.AssignmentID, Limit: 10})
		require.NoError(t, err)
		require.Equal(t, []coordination.Assignment{initial}, history)
	})
	t.Run("lost acknowledgement", func(t *testing.T) {
		ctx, _, h := assignmentFixture(t, backend)
		fault := assurance.New(&assignmentFaultDriver{Driver: driver, plans: plans, afterCommit: true})
		result, err := fault.ProposeAssignment(ctx, h)
		require.ErrorIs(t, err, errAssignmentInjected)
		require.Zero(t, result)
		retained, err := journal.GetAssignment(ctx, h.AssignmentID)
		require.NoError(t, err)
		recovered, err := assurance.New(driver).ProposeAssignment(ctx, h)
		require.NoError(t, err)
		require.Equal(t, retained, recovered)
		change := assignmentChange(retained, coordination.AssignmentAccepted)
		result, err = fault.AdvanceAssignment(ctx, change)
		require.ErrorIs(t, err, errAssignmentInjected)
		require.Zero(t, result)
		recovered, err = assurance.New(driver).AdvanceAssignment(ctx, change)
		require.NoError(t, err)
		current, err := journal.GetAssignment(ctx, h.AssignmentID)
		require.NoError(t, err)
		require.Equal(t, current, recovered)
		history, err := journal.AssignmentHistory(ctx, coordination.AssignmentHistoryRequest{AssignmentID: h.AssignmentID, Limit: 10})
		require.NoError(t, err)
		require.Len(t, history, 2)
	})
	t.Run("expiry", func(t *testing.T) {
		ctx, _, h := assignmentFixture(t, backend)
		clock := &jobClock{at: h.Deadline.Add(-time.Minute)}
		repo := assurance.New(driver).WithClock(clock)
		initial, err := repo.ProposeAssignment(ctx, h)
		require.NoError(t, err)
		clock.advance(time.Minute)
		_, err = repo.AdvanceAssignment(ctx, assignmentChange(initial, coordination.AssignmentAccepted))
		require.ErrorIs(t, err, coordination.ErrInvalidAssignment)
		retry, err := repo.ProposeAssignment(ctx, h)
		require.NoError(t, err)
		require.Equal(t, initial, retry)
		cancelled, err := repo.AdvanceAssignment(ctx, assignmentChange(initial, coordination.AssignmentCancelled))
		require.NoError(t, err)
		require.Equal(t, coordination.AssignmentCancelled, cancelled.State)
		require.Equal(t, clock.Now(), cancelled.At)
	})
	t.Run("plan fence", func(t *testing.T) {
		ctx, p, h := assignmentFixture(t, backend)
		fault := &assignmentFaultDriver{Driver: driver, plans: plans, entered: make(chan struct{}), release: make(chan struct{})}
		var release sync.Once
		defer release.Do(func() { close(fault.release) })
		result := make(chan error, 1)
		go func() { _, err := assurance.New(fault).ProposeAssignment(ctx, h); result <- err }()
		select {
		case <-fault.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("assignment transaction did not reach insert")
		}
		bounded, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
		defer cancel()
		_, err := resultCapability[planning.Store](t, backend).CommitRevision(bounded, planning.Revision{Plan: p, Actor: "host", Reason: "concurrent replan"}, 1)
		require.Error(t, err, "plan writer must not commit while assignment holds the plan lock")
		release.Do(func() { close(fault.release) })
		select {
		case err = <-result:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("assignment transaction did not finish")
		}
		current, err := resultCapability[planning.Store](t, backend).GetPlan(ctx, p.ID)
		require.NoError(t, err)
		require.Equal(t, p, current, "cancelled plan writer must leave no revision")
		initial, err := journal.GetAssignment(ctx, h.AssignmentID)
		require.NoError(t, err)
		_, err = resultCapability[planning.Store](t, backend).CommitRevision(ctx, planning.Revision{Plan: p, Actor: "host", Reason: "replan"}, 1)
		require.NoError(t, err)
		_, err = journal.AdvanceAssignment(ctx, assignmentChange(initial, coordination.AssignmentAccepted))
		require.ErrorIs(t, err, coordination.ErrInvalidHandoff)
	})
}
