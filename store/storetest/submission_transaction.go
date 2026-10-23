package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

type submissionFenceDriver struct {
	assurance.Driver
	plans        assurance.PlanDriver
	entered      chan struct{}
	release      chan struct{}
	once         sync.Once
	afterRead    func()
	beforeFinish func(context.Context, assurance.Driver) error
	failInsert   bool
	lostAck      bool
}

func (d *submissionFenceDriver) TransactPlan(ctx context.Context, identifier id.ID, callback func(context.Context, assurance.Driver) error) error {
	err := d.plans.TransactPlan(ctx, identifier, func(tc context.Context, tx assurance.Driver) error {
		return callback(tc, submissionFenceTx{Driver: tx, owner: d})
	})
	if err == nil && d.lostAck {
		return errAssignmentInjected
	}
	return err
}

type submissionFenceTx struct {
	assurance.Driver
	owner *submissionFenceDriver
}

func (d submissionFenceTx) LockSubmissionRun(ctx context.Context, identifier id.AgentRunID) (*run.Run, error) {
	reader, ok := d.Driver.(assurance.SubmissionRunReader)
	if !ok {
		return nil, coordination.ErrAssignmentUnavailable
	}
	return reader.LockSubmissionRun(ctx, identifier)
}
func (d submissionFenceTx) Select(ctx context.Context, query assurance.Query) ([]assurance.Record, error) {
	rows, err := d.Driver.Select(ctx, query)
	if err != nil || query.Kind != assurance.KindEvidence {
		return rows, err
	}
	if d.owner.afterRead != nil {
		d.owner.afterRead()
	}
	if d.owner.entered != nil {
		d.owner.once.Do(func() { close(d.owner.entered) })
		select {
		case <-d.owner.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if d.owner.beforeFinish != nil {
		if checkErr := d.owner.beforeFinish(ctx, d.Driver); checkErr != nil {
			return nil, checkErr
		}
	}
	return rows, nil
}
func (d submissionFenceTx) Insert(ctx context.Context, row assurance.Record) error {
	if err := d.Driver.Insert(ctx, row); err != nil {
		return err
	}
	if d.owner.failInsert {
		return errAssignmentInjected
	}
	return nil
}

// SubmissionTransactions verifies that evidence and run fences survive until
// the assignment observation commits, and that uncertainty preserves history.
func SubmissionTransactions(t *testing.T, backend store.Store, driver assurance.Driver) {
	plans, ok := driver.(assurance.PlanDriver)
	require.True(t, ok)
	journal := resultCapability[coordination.AssignmentStore](t, backend)
	t.Run("rollback", func(t *testing.T) {
		ctx, working, change, _ := submissionFixture(t, backend)
		faulty := assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, failInsert: true})
		result, err := faulty.AdvanceAssignment(ctx, change)
		require.ErrorIs(t, err, errAssignmentInjected)
		require.Zero(t, result)
		current, err := journal.GetAssignment(ctx, working.Handoff.AssignmentID)
		require.NoError(t, err)
		require.Equal(t, working, current)
	})
	t.Run("lost acknowledgement", func(t *testing.T) {
		ctx, working, change, _ := submissionFixture(t, backend)
		faulty := assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, lostAck: true})
		result, err := faulty.AdvanceAssignment(ctx, change)
		require.ErrorIs(t, err, errAssignmentInjected)
		require.Zero(t, result)
		recovered, err := assurance.New(driver).AdvanceAssignment(ctx, change)
		require.NoError(t, err)
		current, err := journal.GetAssignment(ctx, working.Handoff.AssignmentID)
		require.NoError(t, err)
		require.Equal(t, current, recovered)
		history, err := journal.AssignmentHistory(ctx, coordination.AssignmentHistoryRequest{AssignmentID: working.Handoff.AssignmentID, Limit: 10})
		require.NoError(t, err)
		require.Len(t, history, 4)
	})
	t.Run("expires during evidence read", func(t *testing.T) {
		ctx, working, change, _ := submissionFixture(t, backend)
		clock := &jobClock{at: time.Now().UTC()}
		faulty := assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, afterRead: func() { clock.advance(2 * time.Hour) }}).WithClock(clock)
		result, err := faulty.AdvanceAssignment(ctx, change)
		require.ErrorIs(t, err, coordination.ErrInvalidAssignment)
		require.Zero(t, result)
		current, err := journal.GetAssignment(ctx, working.Handoff.AssignmentID)
		require.NoError(t, err)
		require.Equal(t, working, current)
	})
	t.Run("run and tombstone fence", func(t *testing.T) {
		ctx, _, change, job := submissionFixture(t, backend)
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		current, err := backend.GetRun(ctx, change.Submission.RunID)
		require.NoError(t, err)
		fenced := &submissionFenceDriver{Driver: driver, plans: plans, entered: make(chan struct{}), release: make(chan struct{})}
		storage := resultCapability[evidence.Storage](t, backend)
		fenced.beforeFinish = func(tc context.Context, tx assurance.Driver) error {
			reader, ok := tx.(assurance.RunTransaction)
			if !ok {
				return coordination.ErrAssignmentUnavailable
			}
			held, readErr := reader.GetRun(tc, current.ID)
			if readErr != nil {
				return readErr
			}
			if held.Revision != current.Revision || held.State != current.State {
				return errAssignmentInjected
			}
			_, readErr = assurance.New(tx).LoadEvidence(tc, job.Request.Binding.Candidate)
			return readErr
		}
		var released sync.Once
		defer released.Do(func() { close(fenced.release) })
		result := make(chan error, 1)
		go func() { _, advanceErr := assurance.New(fenced).AdvanceAssignment(ctx, change); result <- advanceErr }()
		select {
		case <-fenced.entered:
		case <-ctx.Done():
			t.Fatal("submission did not reach evidence read")
		}
		bounded, stop := context.WithTimeout(ctx, 250*time.Millisecond)
		defer stop()
		changed := *current
		changed.State = run.StateCancelled
		errs := make(chan error, 2)
		go func() { errs <- backend.UpdateRun(bounded, &changed) }()
		go func() {
			errs <- storage.TombstoneEvidence(evidence.WithDeletionActor(bounded, "host"), job.Request.Binding.Candidate, "revoke")
		}()
		require.Error(t, <-errs, "concurrent update must not commit through a held fence")
		require.Error(t, <-errs, "concurrent update must not commit through a held fence")
		released.Do(func() { close(fenced.release) })
		select {
		case err = <-result:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal("submission did not finish")
		}
		// A timed-out client may leave a statement that runs after lock release.
		// The callback proved both rows unchanged before this transaction committed.
		retained, err := journal.GetAssignment(ctx, change.AssignmentID)
		require.NoError(t, err)
		require.Equal(t, coordination.AssignmentSubmitted, retained.State)
	})
}
