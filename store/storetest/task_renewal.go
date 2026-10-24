package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

func taskRenewalTransactions(t *testing.T, backend store.Store, driver assurance.Driver) {
	plans, ok := driver.(assurance.PlanDriver)
	require.True(t, ok)
	fixture := func(t *testing.T) (context.Context, *assurance.Repository, coordination.TaskRenewalRequest) {
		ctx, creation, _ := taskRunFixture(t, backend, driver)
		creation.Run.Provenance.TaskClaimID = creation.Claim.ClaimID.String()
		repo := assurance.New(driver)
		_, err := repo.CreateTaskRun(ctx, creation)
		require.NoError(t, err)
		claim, err := repo.GetTaskClaim(ctx, creation.Claim.PlanID, creation.Claim.TaskID)
		require.NoError(t, err)
		dispatched, err := repo.GetTaskDispatch(ctx, claim.ID)
		require.NoError(t, err)
		return ctx, repo, coordination.TaskRenewalRequest{Actor: claim.Worker.Principal, Reason: "retain active ownership", SchemaVersion: 1, Claim: claim, ParentRunID: dispatched.Assignment.Handoff.ParentRunID, ParentRunRevision: creation.ParentRunRevision, RunRevision: 1, AssignmentID: dispatched.Assignment.Handoff.AssignmentID, AssignmentRevision: dispatched.Assignment.Revision, AssignmentDigest: dispatched.Assignment.Digest, Duration: 2 * time.Minute}
	}
	t.Run("renew_retry_scope", func(t *testing.T) {
		ctx, repo, req := fixture(t)
		require.NotNil(t, resultCapability[coordination.TaskRenewalStore](t, backend))
		receipt, err := repo.RenewTaskExecution(ctx, req)
		require.NoError(t, err)
		require.True(t, receipt.Fresh)
		require.NoError(t, receipt.Validate(req))
		current, err := repo.GetTaskClaim(ctx, req.Claim.PlanID, req.Claim.TaskID)
		require.NoError(t, err)
		require.Equal(t, receipt.Claim, current)
		again, err := repo.RenewTaskExecution(ctx, req)
		require.NoError(t, err)
		receipt.Fresh = false
		require.Equal(t, receipt, again)
		_, err = repo.RenewTaskExecution(context.Background(), req)
		require.Error(t, err)
		_, err = repo.RenewTaskExecution(ctxWithScope("other"), req)
		require.Error(t, err)
		snapshot, err := repo.GetBudgetSnapshot(ctx, req.Claim.PlanID)
		require.NoError(t, err)
		require.Equal(t, 1, snapshot.Reservations)
		require.EqualValues(t, 600, snapshot.Charged.Tokens)
	})
	for _, mode := range []string{"parent_cancelled", "parent_revision", "child_cancelled", "child_failed", "child_incomplete", "child_revision", "child_claim", "child_parent", "child_prepared", "assignment", "expired", "settled", "deadline", "expired_during_write"} {
		t.Run(mode, func(t *testing.T) {
			ctx, repo, req := fixture(t)
			switch mode {
			case "parent_revision":
				req.ParentRunRevision++
			case "child_revision":
				req.RunRevision++
			case "parent_cancelled":
				parent, err := backend.GetRun(ctx, req.ParentRunID)
				require.NoError(t, err)
				parent.State = run.StateCancelled
				require.NoError(t, backend.UpdateRun(ctx, parent))
				req.ParentRunRevision = parent.Revision
			case "child_cancelled", "child_failed", "child_incomplete", "child_claim", "child_parent", "child_prepared", "deadline":
				child, err := backend.GetRun(ctx, req.Claim.RunID)
				require.NoError(t, err)
				switch mode {
				case "child_cancelled":
					child.State = run.StateCancelled
				case "child_failed":
					child.State = run.StateFailed
				case "child_incomplete":
					child.State = run.StateIncomplete
				case "child_claim":
					child.Provenance.TaskClaimID = id.New(coordination.PrefixClaim).String()
				case "child_parent":
					child.Provenance.ParentRunID = id.NewAgentRunID().String()
				case "child_prepared":
					child.Provenance.PreparedDigest = "changed"
				case "deadline":
					child.Budget.Deadline = req.Claim.ExpiresAt
				}
				require.NoError(t, backend.UpdateRun(ctx, child))
				req.RunRevision = child.Revision
			case "assignment":
				a, err := repo.GetAssignment(ctx, req.AssignmentID)
				require.NoError(t, err)
				a, err = repo.AdvanceAssignment(ctx, assignmentChange(a, coordination.AssignmentCancelled))
				require.NoError(t, err)
				req.AssignmentRevision = a.Revision
				req.AssignmentDigest = a.Digest
			case "expired":
				repo = repo.WithClock(&jobClock{at: req.Claim.ExpiresAt})
			case "settled":
				allocation, err := repo.GetBudgetedTaskClaim(ctx, req.Claim.ID)
				require.NoError(t, err)
				require.NoError(t, repo.Settle(ctx, execution.Settlement{ReservationID: allocation.Reservation.Request.ID, RequestDigest: allocation.Reservation.RequestDigest, Usage: execution.ReservationUsage{}, Receipt: "settled"}))
			case "expired_during_write":
				req.Duration = time.Minute
				clock := &jobClock{at: time.Now().UTC()}
				repo = assurance.New(&claimWriteDriver{Driver: driver, plans: plans, clock: clock, advanceKind: "task_execution_renewal"}).WithClock(clock)
			}
			_, err := repo.RenewTaskExecution(ctx, req)
			require.Error(t, err)
			current, err := assurance.New(driver).GetTaskClaim(ctx, req.Claim.PlanID, req.Claim.TaskID)
			require.NoError(t, err)
			require.Equal(t, req.Claim, current)
		})
	}
	for _, state := range []run.State{run.StatePaused, run.StateCompleted} {
		t.Run("retain_"+string(state), func(t *testing.T) {
			ctx, repo, req := fixture(t)
			child, err := backend.GetRun(ctx, req.Claim.RunID)
			require.NoError(t, err)
			child.State = state
			require.NoError(t, backend.UpdateRun(ctx, child))
			req.RunRevision = child.Revision
			receipt, err := repo.RenewTaskExecution(ctx, req)
			require.NoError(t, err)
			require.True(t, receipt.Fresh)
		})
	}
	t.Run("rollback_outbox", func(t *testing.T) {
		ctx, _, req := fixture(t)
		repo := assurance.New(&claimWriteDriver{Driver: driver, plans: plans, failKind: "task_execution_renewal"})
		_, err := repo.RenewTaskExecution(ctx, req)
		require.ErrorIs(t, err, errAssignmentInjected)
		current, err := assurance.New(driver).GetTaskClaim(ctx, req.Claim.PlanID, req.Claim.TaskID)
		require.NoError(t, err)
		require.Equal(t, req.Claim, current)
		receipt, err := assurance.New(driver).RenewTaskExecution(ctx, req)
		require.NoError(t, err)
		require.True(t, receipt.Fresh)
	})
	t.Run("lost_ack", func(t *testing.T) {
		ctx, repo, req := fixture(t)
		_, err := assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, lostAck: true}).RenewTaskExecution(ctx, req)
		require.ErrorIs(t, err, errAssignmentInjected)
		receipt, err := repo.RenewTaskExecution(ctx, req)
		require.NoError(t, err)
		require.False(t, receipt.Fresh)
		require.NoError(t, receipt.Validate(req))
	})
	for _, distinct := range []bool{false, true} {
		name := "concurrent_same"
		if distinct {
			name = "concurrent_distinct"
		}
		t.Run(name, func(t *testing.T) {
			ctx, repo, req := fixture(t)
			var wg sync.WaitGroup
			results := make(chan coordination.TaskRenewal, 4)
			failures := make(chan error, 4)
			for i := range 4 {
				request, _, err := req.Freeze()
				require.NoError(t, err)
				if distinct {
					request.Duration += time.Duration(i) * time.Second
				}
				wg.Go(func() {
					receipt, callErr := repo.RenewTaskExecution(ctx, request)
					results <- receipt
					failures <- callErr
				})
			}
			wg.Wait()
			close(results)
			close(failures)
			fresh, success := 0, 0
			for receipt := range results {
				if receipt.Fresh {
					fresh++
				}
			}
			for err := range failures {
				if err == nil {
					success++
				} else {
					require.ErrorIs(t, err, coordination.ErrClaimConflict)
				}
			}
			require.Equal(t, 1, fresh)
			if distinct {
				require.Equal(t, 1, success)
			} else {
				require.Equal(t, 4, success)
			}
		})
	}
}
