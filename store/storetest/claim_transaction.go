package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

// ClaimTransactions exercises ownership recovery against the backend's real
// plan transaction. It never starts an agent or an external effect.
func ClaimTransactions(t *testing.T, backend store.Store, driver assurance.Driver) {
	plans, ok := driver.(assurance.PlanDriver)
	require.True(t, ok)
	t.Run("lease_expiry", func(t *testing.T) {
		ctx, request := claimFixture(t, backend)
		clock := &jobClock{at: time.Now().UTC()}
		repo := assurance.New(driver).WithClock(clock)
		claim, err := repo.AcquireTaskClaim(ctx, request)
		require.NoError(t, err)
		clock.advance(time.Minute)
		_, err = repo.BeginTaskDispatch(ctx, claim.Token(), id.NewAgentRunID())
		require.ErrorIs(t, err, coordination.ErrClaimConflict)
		released, err := repo.ExpireTaskClaim(ctx, claim.PlanID, claim.TaskID, claim.Revision)
		require.NoError(t, err)
		require.Equal(t, coordination.ClaimReleased, released.State)
		retry, err := repo.ExpireTaskClaim(ctx, claim.PlanID, claim.TaskID, claim.Revision)
		require.NoError(t, err)
		require.Equal(t, released, retry)
		next := request
		next.ID = id.New(coordination.PrefixClaim)
		next.ExpectedRevision = released.Revision
		successor, err := assurance.New(driver).WithClock(clock).AcquireTaskClaim(ctx, next)
		require.NoError(t, err)
		require.Equal(t, claim.Fence+1, successor.Fence)
		_, err = repo.RenewTaskClaim(ctx, claim.Token(), time.Minute)
		require.ErrorIs(t, err, coordination.ErrClaimConflict)
		dispatch, err := repo.BeginTaskDispatch(ctx, successor.Token(), id.NewAgentRunID())
		require.NoError(t, err)
		clock.advance(time.Minute)
		recovering, err := repo.ExpireTaskClaim(ctx, claim.PlanID, claim.TaskID, dispatch.Claim.Revision)
		require.NoError(t, err)
		require.Equal(t, coordination.ClaimRecovering, recovering.State)
		next.ID = id.New(coordination.PrefixClaim)
		next.ExpectedRevision = recovering.Revision
		_, err = repo.AcquireTaskClaim(ctx, next)
		require.ErrorIs(t, err, coordination.ErrClaimConflict)
	})
	t.Run("rollback", func(t *testing.T) {
		ctx, request := claimFixture(t, backend)
		faulty := assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, failInsert: true})
		claim, err := faulty.AcquireTaskClaim(ctx, request)
		require.ErrorIs(t, err, errAssignmentInjected)
		require.Zero(t, claim)
		_, err = assurance.New(driver).GetTaskClaim(ctx, request.PlanID, request.TaskID)
		require.ErrorIs(t, err, coordination.ErrClaimNotFound)
		_, err = assurance.New(driver).AcquireTaskClaim(ctx, request)
		require.NoError(t, err, "rolled-back identity must not prevent retry")
	})
	t.Run("lost_dispatch_acknowledgement", func(t *testing.T) {
		ctx, request := claimFixture(t, backend)
		repo := assurance.New(driver)
		claim, err := repo.AcquireTaskClaim(ctx, request)
		require.NoError(t, err)
		faulty := assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, lostAck: true})
		child := id.NewAgentRunID()
		dispatch, err := faulty.BeginTaskDispatch(ctx, claim.Token(), child)
		require.ErrorIs(t, err, errAssignmentInjected)
		require.Zero(t, dispatch)
		retry, err := assurance.New(driver).BeginTaskDispatch(ctx, claim.Token(), child)
		require.NoError(t, err)
		require.False(t, retry.Fresh)
		require.Equal(t, child, retry.Claim.RunID)
		require.Equal(t, coordination.ClaimDispatching, retry.Claim.State)
	})
	t.Run("replan_fences_dispatch", func(t *testing.T) {
		ctx, request := claimFixture(t, backend)
		clock := &jobClock{at: time.Now().UTC()}
		repo := assurance.New(driver).WithClock(clock)
		claim, err := repo.AcquireTaskClaim(ctx, request)
		require.NoError(t, err)
		p, err := repo.GetPlan(ctx, request.PlanID)
		require.NoError(t, err)
		p.Tasks[0].ID = id.New(planning.PrefixTask)
		_, err = repo.CommitRevision(ctx, planning.Revision{Plan: p, Actor: "host", Reason: "replan"}, 1)
		require.NoError(t, err)
		_, err = repo.RenewTaskClaim(ctx, claim.Token(), 2*time.Minute)
		require.ErrorIs(t, err, coordination.ErrClaimConflict)
		_, err = repo.BeginTaskDispatch(ctx, claim.Token(), id.NewAgentRunID())
		require.ErrorIs(t, err, coordination.ErrClaimConflict)
		clock.advance(time.Minute)
		released, err := repo.ExpireTaskClaim(ctx, request.PlanID, request.TaskID, claim.Revision)
		require.NoError(t, err)
		require.Equal(t, coordination.ClaimReleased, released.State)
		list, err := repo.ListTaskClaims(ctx, coordination.ClaimListRequest{PlanID: request.PlanID, Limit: 10})
		require.NoError(t, err)
		require.Len(t, list, 1)
		require.Equal(t, request.TaskID, list[0].TaskID, "removed task remains discoverable")
	})
	t.Run("unique_attempt_and_run", func(t *testing.T) {
		ctx, request := claimFixture(t, backend)
		repo := assurance.New(driver)
		first, err := repo.AcquireTaskClaim(ctx, request)
		require.NoError(t, err)
		old, err := repo.GetPlan(ctx, request.PlanID)
		require.NoError(t, err)
		otherPlan := storedPlan(t, old.Scope)
		require.NoError(t, repo.CreatePlan(ctx, otherPlan))
		other := request
		other.PlanID, other.PlanDigest, other.TaskID = otherPlan.ID, otherPlan.Digest, otherPlan.Tasks[0].ID
		_, err = repo.AcquireTaskClaim(ctx, other)
		require.ErrorIs(t, err, coordination.ErrClaimConflict, "attempt identities cannot be reused across plans")
		other.ID = id.New(coordination.PrefixClaim)
		second, err := repo.AcquireTaskClaim(ctx, other)
		require.NoError(t, err)
		child := id.NewAgentRunID()
		_, err = repo.BeginTaskDispatch(ctx, first.Token(), child)
		require.NoError(t, err)
		_, err = repo.BeginTaskDispatch(ctx, second.Token(), child)
		require.ErrorIs(t, err, coordination.ErrClaimConflict, "a child run belongs to one dispatch")
		current, err := repo.GetTaskClaim(ctx, other.PlanID, other.TaskID)
		require.NoError(t, err)
		require.Equal(t, second, current)
	})
	t.Run("dispatch_ack_rollback", func(t *testing.T) {
		ctx, request := claimFixture(t, backend)
		repo := assurance.New(driver)
		claim, err := repo.AcquireTaskClaim(ctx, request)
		require.NoError(t, err)
		faulty := assurance.New(&claimWriteDriver{Driver: driver, plans: plans, failKind: "task_claim_operation"})
		child := id.NewAgentRunID()
		result, err := faulty.BeginTaskDispatch(ctx, claim.Token(), child)
		require.ErrorIs(t, err, errAssignmentInjected)
		require.Zero(t, result)
		retained, err := repo.GetTaskClaim(ctx, request.PlanID, request.TaskID)
		require.NoError(t, err)
		require.Equal(t, claim, retained)
		retry, err := repo.BeginTaskDispatch(ctx, claim.Token(), child)
		require.NoError(t, err)
		require.True(t, retry.Fresh, "rolled-back run binding must not block dispatch")
	})
	t.Run("expiry_before_commit", func(t *testing.T) {
		ctx, request := claimFixture(t, backend)
		clock := &jobClock{at: time.Now().UTC()}
		faulty := assurance.New(&claimWriteDriver{Driver: driver, plans: plans, clock: clock}).WithClock(clock)
		_, err := faulty.AcquireTaskClaim(ctx, request)
		require.ErrorIs(t, err, coordination.ErrClaimConflict)
		_, err = assurance.New(driver).GetTaskClaim(ctx, request.PlanID, request.TaskID)
		require.ErrorIs(t, err, coordination.ErrClaimNotFound)
	})
}

type claimWriteDriver struct {
	assurance.Driver
	plans       assurance.PlanDriver
	clock       *jobClock
	failKind    string
	advanceKind string
}

func (d *claimWriteDriver) TransactPlan(ctx context.Context, planID id.ID, callback func(context.Context, assurance.Driver) error) error {
	return d.plans.TransactPlan(ctx, planID, func(tc context.Context, tx assurance.Driver) error {
		return callback(tc, claimWriteTx{Driver: tx, clock: d.clock, failKind: d.failKind, advanceKind: d.advanceKind})
	})
}

type claimWriteTx struct {
	assurance.Driver
	clock       *jobClock
	failKind    string
	advanceKind string
}

func (d claimWriteTx) Insert(ctx context.Context, row assurance.Record) error {
	err := d.Driver.Insert(ctx, row)
	if err == nil && d.clock != nil && (d.advanceKind == "" || row.Kind == d.advanceKind) {
		d.clock.advance(time.Minute)
	}
	if err == nil && row.Kind == d.failKind {
		return errAssignmentInjected
	}
	return err
}

func (d claimWriteTx) LockSubmissionRun(ctx context.Context, identifier id.AgentRunID) (*run.Run, error) {
	reader, ok := d.Driver.(assurance.SubmissionRunReader)
	if !ok {
		return nil, coordination.ErrAssignmentUnavailable
	}
	return reader.LockSubmissionRun(ctx, identifier)
}

func (d claimWriteTx) GetRun(ctx context.Context, identifier id.AgentRunID) (*run.Run, error) {
	reader, ok := d.Driver.(assurance.TaskRunTransaction)
	if !ok {
		return nil, coordination.ErrAssignmentUnavailable
	}
	return reader.GetRun(ctx, identifier)
}
func (d claimWriteTx) InsertTaskRun(ctx context.Context, child *run.Run) error {
	writer, ok := d.Driver.(assurance.TaskRunTransaction)
	if !ok {
		return coordination.ErrAssignmentUnavailable
	}
	err := writer.InsertTaskRun(ctx, child)
	if err == nil && d.failKind == "task_child_run" {
		return errAssignmentInjected
	}
	return err
}
