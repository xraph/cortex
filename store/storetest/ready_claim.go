package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

func readyClaimFixture(t *testing.T, backend store.Store) (context.Context, coordination.ReadyTaskClaimRequest) {
	t.Helper()
	ctx, acquisition := budgetedClaimFixture(t, backend)
	return ctx, coordination.ReadyTaskClaimRequest{SchemaVersion: 1, Acquisition: acquisition}
}

// ReadyClaimTransactions exercises prerequisite admission and atomic ownership,
// accounting and event retention against each real database driver.
func ReadyClaimTransactions(t *testing.T, backend store.Store, driver assurance.Driver) {
	plans, ok := driver.(assurance.PlanDriver)
	require.True(t, ok)
	t.Run("acquire_retry_scope", func(t *testing.T) {
		ctx, req := readyClaimFixture(t, backend)
		repo := assurance.New(driver)
		require.NotNil(t, resultCapability[coordination.ReadyTaskClaimStore](t, backend))
		result, err := repo.ClaimReadyTask(ctx, req)
		require.NoError(t, err)
		require.True(t, result.Fresh)
		require.False(t, result.Acquisition.Fresh)
		require.Equal(t, req.Acquisition.Claim.ID, result.Outbox.ClaimID)
		require.Equal(t, "task_claimed", result.Outbox.Type)
		retry, err := repo.ClaimReadyTask(ctx, req)
		require.NoError(t, err)
		result.Fresh = false
		require.Equal(t, result, retry)
		loaded, err := repo.GetReadyTaskClaim(ctx, req.Acquisition.Claim.ID)
		require.NoError(t, err)
		require.Equal(t, result, loaded)
		_, err = repo.GetReadyTaskClaim(context.Background(), req.Acquisition.Claim.ID)
		require.ErrorIs(t, err, cortex.ErrNoScope)
		_, err = repo.GetReadyTaskClaim(ctxWithScope("other"), req.Acquisition.Claim.ID)
		require.ErrorIs(t, err, coordination.ErrClaimNotFound)
		req.Acquisition.Claim.GrantRef = "changed"
		_, err = repo.ClaimReadyTask(ctx, req)
		require.ErrorIs(t, err, coordination.ErrClaimConflict)
	})
	for _, kind := range []string{"task_claim_operation", "budget_reservation", "task_claim_budget", "task_ready_claim"} {
		t.Run("rollback_"+kind, func(t *testing.T) {
			ctx, req := readyClaimFixture(t, backend)
			faulty := assurance.New(&claimWriteDriver{Driver: driver, plans: plans, failKind: kind})
			result, err := faulty.ClaimReadyTask(ctx, req)
			require.ErrorIs(t, err, errAssignmentInjected)
			require.Zero(t, result)
			repo := assurance.New(driver)
			_, err = repo.GetTaskClaim(ctx, req.Acquisition.Claim.PlanID, req.Acquisition.Claim.TaskID)
			require.ErrorIs(t, err, coordination.ErrClaimNotFound)
			snapshot, err := repo.GetBudgetSnapshot(ctx, req.Acquisition.Claim.PlanID)
			require.NoError(t, err)
			require.Zero(t, snapshot.Reservations)
			_, err = repo.ClaimReadyTask(ctx, req)
			require.NoError(t, err)
		})
	}
	t.Run("lost_acknowledgment", func(t *testing.T) {
		ctx, req := readyClaimFixture(t, backend)
		result, err := assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, lostAck: true}).ClaimReadyTask(ctx, req)
		require.ErrorIs(t, err, errAssignmentInjected)
		require.Zero(t, result)
		retry, err := assurance.New(driver).ClaimReadyTask(ctx, req)
		require.NoError(t, err)
		require.False(t, retry.Fresh)
	})
	t.Run("cannot_adopt_budgeted_claim", func(t *testing.T) {
		ctx, req := readyClaimFixture(t, backend)
		repo := assurance.New(driver)
		_, err := repo.AcquireBudgetedTaskClaim(ctx, req.Acquisition)
		require.NoError(t, err)
		result, err := repo.ClaimReadyTask(ctx, req)
		require.ErrorIs(t, err, coordination.ErrClaimConflict)
		require.Zero(t, result)
	})
	for _, identical := range []bool{false, true} {
		name := "distinct"
		if identical {
			name = "identical"
		}
		t.Run("concurrent_"+name, func(t *testing.T) {
			ctx, req := readyClaimFixture(t, backend)
			repo := assurance.New(driver)
			var wg sync.WaitGroup
			results := make(chan coordination.ReadyTaskClaim, 2)
			errors := make(chan error, 2)
			for i := 0; i < 2; i++ {
				next, _, err := req.Freeze()
				require.NoError(t, err)
				if !identical && i > 0 {
					next.Acquisition.Claim.ID = id.New(coordination.PrefixClaim)
					next.Acquisition.Reservation.ID = id.New(execution.PrefixReservation)
					next.Acquisition.Reservation.OwnerRef = next.Acquisition.Claim.ID.String()
				}
				wg.Go(func() { value, err := repo.ClaimReadyTask(ctx, next); results <- value; errors <- err })
			}
			wg.Wait()
			close(results)
			close(errors)
			fresh, success := 0, 0
			for value := range results {
				if value.Fresh {
					fresh++
				}
			}
			for err := range errors {
				if err == nil {
					success++
				} else {
					require.ErrorIs(t, err, coordination.ErrClaimConflict)
				}
			}
			require.Equal(t, 1, fresh)
			if identical {
				require.Equal(t, 2, success)
			} else {
				require.Equal(t, 1, success)
			}
		})
	}
	t.Run("capacity_failure", func(t *testing.T) {
		ctx, req := readyClaimFixture(t, backend)
		repo := assurance.New(driver)
		other := req.Acquisition.Reservation
		other.ID = id.New(execution.PrefixReservation)
		_, err := repo.Reserve(ctx, other)
		require.NoError(t, err)
		result, err := repo.ClaimReadyTask(ctx, req)
		require.ErrorIs(t, err, execution.ErrBudgetExceeded)
		require.Zero(t, result)
		_, err = repo.GetTaskClaim(ctx, req.Acquisition.Claim.PlanID, req.Acquisition.Claim.TaskID)
		require.ErrorIs(t, err, coordination.ErrClaimNotFound)
	})
	for _, mode := range []string{"valid", "missing_transitive", "extra_dependency", "expired_ancestor"} {
		t.Run("transitive_"+mode, func(t *testing.T) {
			config := taskContractConfig(t)
			configure := config.Plan
			config.Plan = func(p *planning.Plan) {
				configure(p)
				criterion := p.Contract.Criteria[0]
				criterion.ID = "third"
				p.Contract.Criteria = append(p.Contract.Criteria, criterion)
				third := p.Tasks[1]
				third.ID = id.New(planning.PrefixTask)
				third.Output.Name = "third"
				third.Criteria = []string{"third"}
				third.DependsOn = []id.ID{p.Tasks[1].ID}
				p.Tasks = append(p.Tasks, third)
			}
			ctx, root := taskAcceptanceFixture(t, backend, config)
			repo := assurance.New(driver)
			rootAccepted, err := repo.AcceptTaskResult(ctx, root)
			require.NoError(t, err)
			parent := dependentTaskAcceptanceFixture(ctx, t, backend, root, 1, []evidence.Ref{root.Current.Candidate})
			rootDep := coordination.AcceptedDependency{AcceptanceID: root.ID, Digest: rootAccepted.Digest, Current: root.Current}
			parent.Dependencies = []coordination.AcceptedDependency{rootDep}
			parentAccepted, err := repo.AcceptTaskResult(ctx, parent)
			require.NoError(t, err)
			p, err := repo.GetPlan(ctx, root.Claim.PlanID)
			require.NoError(t, err)
			deps := []coordination.AcceptedDependency{{AcceptanceID: parent.ID, Digest: parentAccepted.Digest, Current: parent.Current}, rootDep}
			req := readyClaimForTask(ctx, t, repo, p, p.Tasks[2].ID, root.Claim.Worker, deps)
			switch mode {
			case "missing_transitive":
				req.Dependencies = req.Dependencies[:1]
			case "extra_dependency":
				req.Acquisition.Claim.TaskID = p.Tasks[1].ID
				req.Acquisition.Reservation.TaskID = p.Tasks[1].ID
			case "expired_ancestor":
				repo = repo.WithClock(&jobClock{at: time.Now().UTC().Add(3 * time.Minute)})
			}
			result, err := repo.ClaimReadyTask(ctx, req)
			if mode == "valid" {
				require.NoError(t, err)
				require.True(t, result.Fresh)
			} else {
				require.Error(t, err)
				require.Zero(t, result)
			}
		})
	}

	for _, mode := range []string{"valid", "missing", "changed_context", "deleted", "expired", "expiry_after_write", "replan", "historical_retry", "unaccepted", "cancelled_parent", "changed_digest", "wrong_scope", "cancelled_context", "expiry_during_read"} {
		t.Run("dependency_"+mode, func(t *testing.T) {
			ctx, root := taskAcceptanceFixture(t, backend)
			repo := assurance.New(driver)
			accepted, err := repo.AcceptTaskResult(ctx, root)
			require.NoError(t, err)
			p, err := repo.GetPlan(ctx, root.Claim.PlanID)
			require.NoError(t, err)
			req := readyClaimForTask(ctx, t, repo, p, p.Tasks[1].ID, root.Claim.Worker, []coordination.AcceptedDependency{{AcceptanceID: root.ID, Digest: accepted.Digest, Current: root.Current}})
			claim := req.Acquisition.Claim
			switch mode {
			case "missing":
				req.Dependencies = nil
			case "changed_context":
				req.Dependencies[0].Current.Context.AuthorityRevision = "revoked"
			case "deleted":
				require.NoError(t, repo.TombstoneEvidence(evidence.WithDeletionActor(ctx, "host"), root.Current.Candidate, "deleted"))
			case "expired":
				repo = repo.WithClock(&jobClock{at: time.Now().UTC().Add(3 * time.Minute)})
			case "expiry_after_write":
				clock := &jobClock{at: time.Now().UTC().Add(90 * time.Second)}
				req.Acquisition.Claim.Duration = 5 * time.Minute
				repo = assurance.New(&claimWriteDriver{Driver: driver, plans: plans, clock: clock, advanceKind: "task_ready_claim"}).WithClock(clock)
			case "replan":
				_, err = repo.CommitRevision(ctx, planning.Revision{Plan: p, Actor: "host", Reason: "new"}, p.Revision)
				require.NoError(t, err)
			case "historical_retry":
				_, err = repo.ClaimReadyTask(ctx, req)
				require.NoError(t, err)
				require.NoError(t, repo.TombstoneEvidence(evidence.WithDeletionActor(ctx, "host"), root.Current.Candidate, "deleted"))
			case "unaccepted":
				req.Dependencies[0].AcceptanceID = id.New(coordination.PrefixTaskAcceptance)
			case "cancelled_parent":
				a, readErr := repo.GetAssignment(ctx, root.AssignmentID)
				require.NoError(t, readErr)
				_, err = repo.AdvanceAssignment(ctx, assignmentChange(a, coordination.AssignmentCancelled))
				require.NoError(t, err)
			case "changed_digest":
				req.Dependencies[0].Digest = evidence.Digest([]byte("other"))
			case "wrong_scope":
				other := ctxWithScope("other")
				_, err = repo.ClaimReadyTask(other, req)
				require.Error(t, err)
				return
			case "cancelled_context":
				bounded, cancel := context.WithCancel(ctx)
				cancel()
				_, err = repo.ClaimReadyTask(bounded, req)
				require.ErrorIs(t, err, context.Canceled)
				return
			case "expiry_during_read":
				clock := &jobClock{at: time.Now().UTC()}
				repo = assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, afterRead: func() { clock.advance(3 * time.Minute) }}).WithClock(clock)
			}
			result, err := repo.ClaimReadyTask(ctx, req)
			if mode == "valid" || mode == "historical_retry" {
				require.NoError(t, err)
				require.Equal(t, mode == "valid", result.Fresh)
				return
			}
			require.Error(t, err)
			require.Zero(t, result)
			_, err = repo.GetTaskClaim(ctx, claim.PlanID, claim.TaskID)
			require.ErrorIs(t, err, coordination.ErrClaimNotFound)
			snapshot, err := repo.GetBudgetSnapshot(ctx, claim.PlanID)
			require.NoError(t, err)
			require.Zero(t, snapshot.Reservations)
		})
	}
}

func readyClaimForTask(ctx context.Context, t *testing.T, repo *assurance.Repository, p planning.Plan, taskID id.ID, worker coordination.Identity, deps []coordination.AcceptedDependency) coordination.ReadyTaskClaimRequest {
	t.Helper()
	policy := execution.ReservationPolicy{SchemaVersion: 1, PlanID: p.ID, Scope: p.Scope, Limits: execution.Resources{Tokens: 100, FanOut: 10}, Deadline: p.Budget.Deadline, TokenOnly: true}
	require.NoError(t, repo.CreateReservationPolicy(ctx, policy))
	claim := coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: taskID, Worker: worker, GrantRef: "grant", Duration: time.Minute}
	req := coordination.ReadyTaskClaimRequest{SchemaVersion: 1, Acquisition: coordination.BudgetedClaimRequest{Claim: claim, Reservation: execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.ID, PlanDigest: p.Digest, Scope: p.Scope, TaskID: claim.TaskID, OwnerRef: claim.ID.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{Tokens: 10, FanOut: 1}, Deadline: p.Budget.Deadline}}, Dependencies: deps}
	return req
}
