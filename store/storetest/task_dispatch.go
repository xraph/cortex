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
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

func dispatchFixture(ctx context.Context, t *testing.T, backend store.Store, repo *assurance.Repository, admission coordination.ReadyTaskClaimRequest) (coordination.TaskDispatchRequest, *run.Run) {
	t.Helper()
	ready, err := repo.ClaimReadyTask(ctx, admission)
	require.NoError(t, err)
	claim := ready.Acquisition.Claim
	p, err := repo.GetPlan(ctx, claim.PlanID)
	require.NoError(t, err)
	authority, err := planning.TaskAuthority(p, claim.TaskID)
	require.NoError(t, err)
	parent := &run.Run{ID: id.NewAgentRunID(), AgentID: mustCreateAgent(t, backend, ctx, ""), State: run.StateRunning}
	require.NoError(t, backend.CreateRun(ctx, parent))
	var criteria []string
	for _, task := range p.Tasks {
		if task.ID == claim.TaskID {
			criteria = task.Criteria
		}
	}
	h, err := coordination.SealHandoff(coordination.Handoff{SchemaVersion: 1, ID: id.New(coordination.PrefixHandoff), AssignmentID: id.New(coordination.PrefixAssignment), Fence: claim.Fence, Scope: p.Scope, PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: claim.TaskID, ParentRunID: parent.ID, Sender: coordination.Identity{Agent: parent.AgentID.String(), Principal: "host"}, Receiver: claim.Worker, Purpose: coordination.HandoffWork, GrantRef: claim.GrantRef, Authority: authority, ContractDigest: p.Contract.Digest, Criteria: criteria, Deadline: authority.Budget.Deadline})
	require.NoError(t, err)
	a, err := repo.ProposeAssignment(ctx, h)
	require.NoError(t, err)
	a, err = repo.AdvanceAssignment(ctx, assignmentChange(a, coordination.AssignmentAccepted))
	require.NoError(t, err)
	return coordination.TaskDispatchRequest{SchemaVersion: 1, Scope: p.Scope, Claim: claim.Token(), ClaimDigest: claim.Digest, AdmissionDigest: ready.RequestDigest, AssignmentID: h.AssignmentID, AssignmentRevision: a.Revision, AssignmentDigest: a.Digest, ParentRunRevision: parent.Revision, RunID: id.NewAgentRunID(), Dependencies: admission.Dependencies, Actor: claim.Worker.Principal, Reason: "dispatch"}, parent
}

// TaskDispatchTransactions verifies readiness, ownership and assignment changes
// commit together without invoking a worker or treating a retry as permission.
func TaskDispatchTransactions(t *testing.T, backend store.Store, driver assurance.Driver) {
	plans, ok := driver.(assurance.PlanDriver)
	require.True(t, ok)
	fixture := func(t *testing.T) (context.Context, *assurance.Repository, coordination.TaskDispatchRequest, *run.Run) {
		ctx, admission := readyClaimFixture(t, backend)
		repo := assurance.New(driver)
		req, parent := dispatchFixture(ctx, t, backend, repo, admission)
		return ctx, repo, req, parent
	}
	t.Run("dispatch_retry_scope", func(t *testing.T) {
		ctx, repo, req, parent := fixture(t)
		require.NotNil(t, resultCapability[coordination.TaskDispatchStore](t, backend))
		result, err := repo.DispatchReadyTask(ctx, req)
		require.NoError(t, err)
		require.True(t, result.Fresh)
		require.NoError(t, result.Validate(req))
		require.Equal(t, coordination.ClaimDispatching, result.Claim.State)
		require.Equal(t, coordination.AssignmentWorking, result.Assignment.State)
		require.Equal(t, req.RunID, result.Claim.RunID)
		_, err = backend.GetRun(ctx, req.RunID)
		require.ErrorIs(t, err, cortex.ErrRunNotFound, "dispatch intent must not fabricate execution")
		parent.State = run.StateCancelled
		require.NoError(t, backend.UpdateRun(ctx, parent))
		retry, err := repo.DispatchReadyTask(ctx, req)
		require.NoError(t, err)
		result.Fresh = false
		require.Equal(t, result, retry)
		loaded, err := repo.GetTaskDispatch(ctx, req.Claim.ClaimID)
		require.NoError(t, err)
		require.Equal(t, result, loaded)
		req.RunID = id.NewAgentRunID()
		_, err = repo.DispatchReadyTask(ctx, req)
		require.ErrorIs(t, err, coordination.ErrClaimConflict)
		_, err = repo.GetTaskDispatch(context.Background(), req.Claim.ClaimID)
		require.ErrorIs(t, err, cortex.ErrNoScope)
		_, err = repo.GetTaskDispatch(ctxWithScope("other"), req.Claim.ClaimID)
		require.ErrorIs(t, err, coordination.ErrClaimNotFound)
	})
	t.Run("renewed_claim_paused_parent", func(t *testing.T) {
		ctx, repo, req, parent := fixture(t)
		renewed, err := repo.RenewTaskClaim(ctx, req.Claim, 2*time.Minute)
		require.NoError(t, err)
		req.Claim, req.ClaimDigest = renewed.Token(), renewed.Digest
		parent.State = run.StatePaused
		require.NoError(t, backend.UpdateRun(ctx, parent))
		req.ParentRunRevision = parent.Revision
		result, err := repo.DispatchReadyTask(ctx, req)
		require.NoError(t, err)
		require.True(t, result.Fresh)
		require.NoError(t, result.Validate(req))
		retry, err := repo.DispatchReadyTask(ctx, req)
		require.NoError(t, err)
		result.Fresh = false
		require.Equal(t, result, retry)
	})
	for _, kind := range []string{"task_dispatch", "task_claim_revision", "task_claim_operation", "assignment_revision", "task_ready_dispatch"} {
		t.Run("rollback_"+kind, func(t *testing.T) {
			ctx, repo, req, _ := fixture(t)
			faulty := assurance.New(&claimWriteDriver{Driver: driver, plans: plans, failKind: kind})
			result, err := faulty.DispatchReadyTask(ctx, req)
			require.ErrorIs(t, err, errAssignmentInjected)
			require.Zero(t, result)
			claim, err := repo.GetTaskClaim(ctx, req.Claim.PlanID, req.Claim.TaskID)
			require.NoError(t, err)
			require.Equal(t, req.Claim, claim.Token())
			a, err := repo.GetAssignment(ctx, req.AssignmentID)
			require.NoError(t, err)
			require.Equal(t, req.AssignmentDigest, a.Digest)
			_, err = repo.DispatchReadyTask(ctx, req)
			require.NoError(t, err)
		})
	}
	t.Run("lost_acknowledgment", func(t *testing.T) {
		ctx, repo, req, _ := fixture(t)
		result, err := assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, lostAck: true}).DispatchReadyTask(ctx, req)
		require.ErrorIs(t, err, errAssignmentInjected)
		require.Zero(t, result)
		retry, err := repo.DispatchReadyTask(ctx, req)
		require.NoError(t, err)
		require.False(t, retry.Fresh)
	})
	for _, mode := range []string{"cancelled_parent", "completed_parent", "parent_revision", "parent_deadline", "cancelled_assignment", "working_assignment", "claim_revision", "claim_digest", "admission_digest", "assignment_digest", "assignment_revision", "actor", "parent_as_child", "replan", "raw_dispatch", "expired", "expiry_after_write", "wrong_scope", "cancelled_context"} {
		t.Run(mode, func(t *testing.T) {
			ctx, repo, req, parent := fixture(t)
			switch mode {
			case "cancelled_parent", "completed_parent":
				parent.State = run.StateCancelled
				if mode == "completed_parent" {
					parent.State = run.StateCompleted
				}
				require.NoError(t, backend.UpdateRun(ctx, parent))
				req.ParentRunRevision = parent.Revision
			case "parent_revision":
				req.ParentRunRevision++
			case "parent_deadline":
				parent.Budget.Deadline = time.Now().UTC().Add(time.Second)
				require.NoError(t, backend.UpdateRun(ctx, parent))
				req.ParentRunRevision = parent.Revision
			case "cancelled_assignment", "working_assignment":
				a, err := repo.GetAssignment(ctx, req.AssignmentID)
				require.NoError(t, err)
				state := coordination.AssignmentCancelled
				if mode == "working_assignment" {
					state = coordination.AssignmentWorking
				}
				a, err = repo.AdvanceAssignment(ctx, assignmentChange(a, state))
				require.NoError(t, err)
				req.AssignmentRevision, req.AssignmentDigest = a.Revision, a.Digest
			case "claim_revision":
				req.Claim.Revision++
			case "claim_digest":
				req.ClaimDigest = evidence.Digest([]byte("other"))
			case "admission_digest":
				req.AdmissionDigest = evidence.Digest([]byte("other"))
			case "assignment_digest":
				req.AssignmentDigest = evidence.Digest([]byte("other"))
			case "assignment_revision":
				req.AssignmentRevision++
			case "actor":
				req.Actor = "other"
			case "parent_as_child":
				req.RunID = parent.ID
			case "replan":
				p, err := repo.GetPlan(ctx, req.Claim.PlanID)
				require.NoError(t, err)
				_, err = repo.CommitRevision(ctx, planning.Revision{Plan: p, Actor: "host", Reason: "changed"}, p.Revision)
				require.NoError(t, err)
			case "raw_dispatch":
				_, err := repo.BeginTaskDispatch(ctx, req.Claim, req.RunID)
				require.NoError(t, err)
			case "expired":
				repo = repo.WithClock(&jobClock{at: time.Now().UTC().Add(2 * time.Minute)})
			case "expiry_after_write":
				clock := &jobClock{at: time.Now().UTC()}
				repo = assurance.New(&claimWriteDriver{Driver: driver, plans: plans, clock: clock, advanceKind: "task_ready_dispatch"}).WithClock(clock)
			case "wrong_scope":
				ctx = ctxWithScope("other")
			case "cancelled_context":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			result, err := repo.DispatchReadyTask(ctx, req)
			require.Error(t, err)
			require.Zero(t, result)
		})
	}
	for _, identical := range []bool{false, true} {
		name := "distinct"
		if identical {
			name = "identical"
		}
		t.Run("concurrent_"+name, func(t *testing.T) {
			ctx, repo, req, _ := fixture(t)
			var wg sync.WaitGroup
			results := make(chan coordination.TaskDispatch, 2)
			failures := make(chan error, 2)
			for i := 0; i < 2; i++ {
				next, _, err := req.Freeze()
				require.NoError(t, err)
				if i > 0 && !identical {
					next.RunID = id.NewAgentRunID()
				}
				wg.Go(func() { result, err := repo.DispatchReadyTask(ctx, next); results <- result; failures <- err })
			}
			wg.Wait()
			close(results)
			close(failures)
			fresh, success := 0, 0
			for result := range results {
				if result.Fresh {
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
			expected := 1
			if identical {
				expected = 2
			}
			require.Equal(t, expected, success)
		})
	}
	for _, mode := range []string{"valid", "missing", "changed_context", "deleted", "cancelled", "expired_after_write"} {
		t.Run("dependency_"+mode, func(t *testing.T) {
			ctx, root := taskAcceptanceFixture(t, backend)
			repo := assurance.New(driver)
			accepted, err := repo.AcceptTaskResult(ctx, root)
			require.NoError(t, err)
			p, err := repo.GetPlan(ctx, root.Claim.PlanID)
			require.NoError(t, err)
			admission := readyClaimForTask(ctx, t, repo, p, p.Tasks[1].ID, root.Claim.Worker, []coordination.AcceptedDependency{{AcceptanceID: root.ID, Digest: accepted.Digest, Current: root.Current}})
			admission.Acquisition.Claim.Duration = 5 * time.Minute
			req, _ := dispatchFixture(ctx, t, backend, repo, admission)
			switch mode {
			case "missing":
				req.Dependencies = nil
			case "changed_context":
				req.Dependencies[0].Current.Context.AuthorityRevision = "changed"
			case "deleted":
				require.NoError(t, repo.TombstoneEvidence(evidence.WithDeletionActor(ctx, "host"), root.Current.Candidate, "deleted"))
			case "cancelled":
				a, readErr := repo.GetAssignment(ctx, root.AssignmentID)
				require.NoError(t, readErr)
				_, err = repo.AdvanceAssignment(ctx, assignmentChange(a, coordination.AssignmentCancelled))
				require.NoError(t, err)
			case "expired_after_write":
				clock := &jobClock{at: time.Now().UTC().Add(90 * time.Second)}
				repo = assurance.New(&claimWriteDriver{Driver: driver, plans: plans, clock: clock, advanceKind: "task_ready_dispatch"}).WithClock(clock)
			}
			result, err := repo.DispatchReadyTask(ctx, req)
			if mode == "valid" {
				require.NoError(t, err)
				require.True(t, result.Fresh)
			} else {
				require.Error(t, err)
				require.Zero(t, result)
			}
		})
	}
}
