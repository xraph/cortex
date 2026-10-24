package storetest

import (
	"context"
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
	"github.com/xraph/cortex/verification"
)

func taskAcceptanceFixture(t *testing.T, backend store.Store, configs ...submissionFixtureConfig) (context.Context, coordination.TaskAcceptanceRequest) {
	t.Helper()
	config := taskContractConfig(t)
	if len(configs) > 0 {
		config = configs[0]
	}
	ctx, working, change, job := configuredSubmissionFixture(t, backend, config)
	return ctx, finishTaskAcceptanceFixture(ctx, t, backend, working, change, job)
}

type acceptanceClaimFixture struct {
	ID       id.ID
	Duration time.Duration
}

func finishTaskAcceptanceFixture(ctx context.Context, t *testing.T, backend store.Store, working coordination.Assignment, change coordination.AssignmentChange, job verification.Job, options ...acceptanceClaimFixture) coordination.TaskAcceptanceRequest {
	t.Helper()
	claims := resultCapability[coordination.ClaimStore](t, backend)
	h := working.Handoff
	fixture := acceptanceClaimFixture{ID: id.New(coordination.PrefixClaim), Duration: 5 * time.Minute}
	if len(options) > 0 {
		fixture = options[0]
	}
	claim, err := claims.AcquireTaskClaim(ctx, coordination.ClaimRequest{SchemaVersion: 1, ID: fixture.ID, PlanID: h.PlanID, PlanRevision: h.PlanRevision, PlanDigest: h.PlanDigest, TaskID: h.TaskID, Worker: h.Receiver, GrantRef: h.GrantRef, Duration: fixture.Duration})
	require.NoError(t, err)
	dispatch, err := claims.BeginTaskDispatch(ctx, claim.Token(), job.Request.Binding.RunID)
	require.NoError(t, err)
	assignment, err := resultCapability[coordination.AssignmentStore](t, backend).AdvanceAssignment(ctx, change)
	require.NoError(t, err)
	jobs := resultCapability[verification.JobStore](t, backend)
	job, err = jobs.ClaimVerificationJob(ctx, verification.JobClaim{JobID: job.Request.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "verifier", Duration: 5 * time.Minute})
	require.NoError(t, err)
	now := time.Now().UTC()
	contract, err := resultCapability[verification.Store](t, backend).GetContract(ctx, job.Request.ContractID)
	require.NoError(t, err)
	outcomes := make([]verification.Outcome, 0, len(contract.Criteria))
	for _, c := range contract.Criteria {
		outcomes = append(outcomes, verification.Outcome{CriterionID: c.ID, Verifier: c.Verifier, VerifierRevision: c.VerifierRevision, Status: verification.StatusPassed})
	}
	report, err := verification.SealReport(verification.Report{SchemaVersion: 1, ID: id.New(verification.PrefixReport), Binding: job.Request.Binding, CheckedAt: now, ExpiresAt: now.Add(2 * time.Minute), Outcomes: outcomes})
	require.NoError(t, err)
	job, err = jobs.SaveVerificationJobReport(ctx, *job.Lease, report)
	require.NoError(t, err)
	receipt, err := resultCapability[verification.CommitStore](t, backend).CommitVerifiedResult(ctx, jobCommitRequest(job))
	require.NoError(t, err)
	return coordination.TaskAcceptanceRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixTaskAcceptance), Claim: dispatch.Claim.Token(), AssignmentID: assignment.Handoff.AssignmentID, AssignmentRevision: assignment.Revision, AssignmentDigest: assignment.Digest, Result: verification.ResultReadRequest{SchemaVersion: 1, RunID: job.Request.Binding.RunID, ResultID: receipt.Request.ResultID, ReceiptDigest: receipt.Digest}, Current: job.Request.Binding, Actor: h.Sender.Principal, Reason: "required evidence passed"}
}

func testTaskAcceptance(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx, req := taskAcceptanceFixture(t, backend)
	s := resultCapability[coordination.TaskAcceptanceStore](t, backend)
	accepted, err := s.AcceptTaskResult(ctx, req)
	require.NoError(t, err)
	require.True(t, accepted.Fresh)
	require.NoError(t, accepted.Validate())
	require.Equal(t, coordination.ClaimCompleted, accepted.Claim.State)
	require.Equal(t, req.ID, *accepted.Claim.AcceptanceID)
	claim, err := resultCapability[coordination.ClaimStore](t, backend).GetTaskClaim(ctx, req.Claim.PlanID, req.Claim.TaskID)
	require.NoError(t, err)
	require.Equal(t, accepted.Claim, claim)
	retry, err := s.AcceptTaskResult(ctx, req)
	require.NoError(t, err)
	accepted.Fresh = false
	require.Equal(t, accepted, retry)
	loaded, err := s.GetTaskAcceptance(ctx, req.ID)
	require.NoError(t, err)
	require.Equal(t, accepted, loaded)
	listed, err := s.ListTaskAcceptances(ctx, coordination.TaskAcceptanceListRequest{PlanID: req.Claim.PlanID, Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []coordination.TaskAcceptance{accepted}, listed)
	listed, err = s.ListTaskAcceptances(ctx, coordination.TaskAcceptanceListRequest{PlanID: req.Claim.PlanID, After: req.ID, Limit: 1})
	require.NoError(t, err)
	require.Empty(t, listed)
	_, err = s.ListTaskAcceptances(context.Background(), coordination.TaskAcceptanceListRequest{PlanID: req.Claim.PlanID, Limit: 1})
	require.ErrorIs(t, err, cortex.ErrNoScope)
	listed, err = s.ListTaskAcceptances(ctxWithScope("other"), coordination.TaskAcceptanceListRequest{PlanID: req.Claim.PlanID, Limit: 1})
	require.NoError(t, err)
	require.Empty(t, listed)
	changed := req
	changed.Reason = "changed request"
	_, err = s.AcceptTaskResult(ctx, changed)
	require.ErrorIs(t, err, coordination.ErrTaskAcceptanceConflict)
}

func testTaskAcceptanceDenied(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	for _, mode := range []string{"stale_claim", "changed_assignment", "changed_result", "changed_context", "wrong_actor", "cancelled_assignment", "changed_run", "deleted_evidence", "replan", "missing_dependency", "extra_dependency", "scope", "cancelled_context"} {
		t.Run(mode, func(t *testing.T) {
			ctx, req := taskAcceptanceFixture(t, backend)
			s := resultCapability[coordination.TaskAcceptanceStore](t, backend)
			readCtx := ctx
			switch mode {
			case "stale_claim":
				req.Claim.Revision--
			case "changed_assignment":
				req.AssignmentDigest = evidence.Digest([]byte("other"))
			case "changed_result":
				req.Result.ReceiptDigest = evidence.Digest([]byte("other"))
			case "changed_context":
				req.Current.Context.AuthorityRevision = "revoked"
			case "wrong_actor":
				req.Actor = "peer"
			case "cancelled_assignment":
				journal := resultCapability[coordination.AssignmentStore](t, backend)
				a, err := journal.GetAssignment(ctx, req.AssignmentID)
				require.NoError(t, err)
				_, err = journal.AdvanceAssignment(ctx, assignmentChange(a, coordination.AssignmentCancelled))
				require.NoError(t, err)
			case "changed_run":
				r, err := backend.GetRun(ctx, req.Result.RunID)
				require.NoError(t, err)
				r.State = run.StateCancelled
				require.NoError(t, backend.UpdateRun(ctx, r))
			case "deleted_evidence":
				require.NoError(t, resultCapability[evidence.Storage](t, backend).TombstoneEvidence(evidence.WithDeletionActor(ctx, "host"), req.Current.Candidate, "revoked"))
			case "replan":
				plans := resultCapability[planning.Store](t, backend)
				p, err := plans.GetPlan(ctx, req.Claim.PlanID)
				require.NoError(t, err)
				_, err = plans.CommitRevision(ctx, planning.Revision{Plan: p, Actor: "host", Reason: "new"}, p.Revision)
				require.NoError(t, err)
			case "missing_dependency":
				req.Dependencies = []coordination.AcceptedDependency{{AcceptanceID: id.New(coordination.PrefixTaskAcceptance), Digest: evidence.Digest([]byte("missing")), Current: req.Current}}
			case "extra_dependency":
				req.Dependencies = []coordination.AcceptedDependency{{AcceptanceID: req.ID, Digest: evidence.Digest([]byte("self")), Current: req.Current}}
			case "scope":
				ctx = context.Background()
			case "cancelled_context":
				bounded, cancel := context.WithCancel(ctx)
				cancel()
				ctx = bounded
			}
			accepted, err := s.AcceptTaskResult(ctx, req)
			require.Error(t, err)
			require.Zero(t, accepted)
			_, err = s.GetTaskAcceptance(readCtx, req.ID)
			require.ErrorIs(t, err, coordination.ErrTaskAcceptanceNotFound)
		})
	}
}

// TaskAcceptanceTransactions checks rollback and time fences on real backends.
func TaskAcceptanceTransactions(t *testing.T, backend store.Store, driver assurance.Driver) {
	plans, ok := driver.(assurance.PlanDriver)
	require.True(t, ok)
	for _, kind := range []string{"task_claim_revision", "task_acceptance", "accepted_task"} {
		t.Run("rollback_"+kind, func(t *testing.T) {
			ctx, req := taskAcceptanceFixture(t, backend)
			faulty := assurance.New(&claimWriteDriver{Driver: driver, plans: plans, failKind: kind})
			_, err := faulty.AcceptTaskResult(ctx, req)
			require.ErrorIs(t, err, errAssignmentInjected)
			repo := assurance.New(driver)
			claim, err := repo.GetTaskClaim(ctx, req.Claim.PlanID, req.Claim.TaskID)
			require.NoError(t, err)
			require.Equal(t, req.Claim, claim.Token())
			_, err = repo.GetTaskAcceptance(ctx, req.ID)
			require.ErrorIs(t, err, coordination.ErrTaskAcceptanceNotFound)
			_, err = repo.AcceptTaskResult(ctx, req)
			require.NoError(t, err)
		})
	}
	for _, sameRequest := range []bool{false, true} {
		name := "competing_acceptances"
		if sameRequest {
			name = "concurrent_retry"
		}
		t.Run(name, func(t *testing.T) {
			ctx, req := taskAcceptanceFixture(t, backend)
			repo := assurance.New(driver)
			other := req
			if !sameRequest {
				other.ID = id.New(coordination.PrefixTaskAcceptance)
			}
			type outcome struct {
				receipt coordination.TaskAcceptance
				err     error
			}
			results := make(chan outcome, 2)
			start := make(chan struct{})
			for _, request := range []coordination.TaskAcceptanceRequest{req, other} {
				go func() { <-start; receipt, err := repo.AcceptTaskResult(ctx, request); results <- outcome{receipt, err} }()
			}
			close(start)
			fresh, successes := 0, 0
			for range 2 {
				result := <-results
				if result.err != nil {
					require.ErrorIs(t, result.err, coordination.ErrTaskAcceptanceConflict)
					continue
				}
				successes++
				if result.receipt.Fresh {
					fresh++
				}
			}
			require.Equal(t, 1, fresh)
			if sameRequest {
				require.Equal(t, 2, successes)
			} else {
				require.Equal(t, 1, successes)
			}
			listed, err := repo.ListTaskAcceptances(ctx, coordination.TaskAcceptanceListRequest{PlanID: req.Claim.PlanID, Limit: 10})
			require.NoError(t, err)
			require.Len(t, listed, 1)
			claim, err := repo.GetTaskClaim(ctx, req.Claim.PlanID, req.Claim.TaskID)
			require.NoError(t, err)
			require.Equal(t, listed[0].Claim, claim)
		})
	}
	t.Run("expiry_after_final_write", func(t *testing.T) {
		ctx, req := taskAcceptanceFixture(t, backend)
		clock := &jobClock{at: time.Now().UTC().Add(90 * time.Second)}
		faulty := assurance.New(&claimWriteDriver{Driver: driver, plans: plans, clock: clock, advanceKind: "accepted_task"}).WithClock(clock)
		result, err := faulty.AcceptTaskResult(ctx, req)
		require.Error(t, err, "report expires after the final insert and must roll back")
		require.Zero(t, result)
		repo := assurance.New(driver)
		claim, err := repo.GetTaskClaim(ctx, req.Claim.PlanID, req.Claim.TaskID)
		require.NoError(t, err)
		require.Equal(t, req.Claim, claim.Token())
		_, err = repo.GetTaskAcceptance(ctx, req.ID)
		require.ErrorIs(t, err, coordination.ErrTaskAcceptanceNotFound)
		accepted, err := repo.AcceptTaskResult(ctx, req)
		require.NoError(t, err)
		require.True(t, accepted.Fresh)
	})
	t.Run("lost_acknowledgment", func(t *testing.T) {
		ctx, req := taskAcceptanceFixture(t, backend)
		faulty := assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, lostAck: true})
		result, err := faulty.AcceptTaskResult(ctx, req)
		require.ErrorIs(t, err, errAssignmentInjected)
		require.Zero(t, result)
		accepted, err := assurance.New(driver).AcceptTaskResult(ctx, req)
		require.NoError(t, err)
		require.False(t, accepted.Fresh)
	})
	t.Run("renew_after_verification", func(t *testing.T) {
		ctx, req := taskAcceptanceFixture(t, backend)
		repo := assurance.New(driver)
		renewed, err := repo.RenewTaskClaim(ctx, req.Claim, 6*time.Minute)
		require.NoError(t, err)
		req.Claim = renewed.Token()
		_, err = repo.AcceptTaskResult(ctx, req)
		require.NoError(t, err, "renewal after verification must preserve the original dispatch time")
	})
	for _, mode := range []string{"expired_report", "expired_claim", "expiry_during_read"} {
		t.Run(mode, func(t *testing.T) {
			ctx, req := taskAcceptanceFixture(t, backend)
			clock := &jobClock{at: time.Now().UTC()}
			repo := assurance.New(driver).WithClock(clock)
			switch mode {
			case "expired_report":
				clock.advance(3 * time.Minute)
			case "expired_claim":
				clock.advance(6 * time.Minute)
			case "expiry_during_read":
				repo = assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, afterRead: func() { clock.advance(3 * time.Minute) }}).WithClock(clock)
			}
			_, err := repo.AcceptTaskResult(ctx, req)
			require.Error(t, err)
			_, err = assurance.New(driver).GetTaskAcceptance(ctx, req.ID)
			require.ErrorIs(t, err, coordination.ErrTaskAcceptanceNotFound)
		})
	}
	t.Run("handoff_deadline_fences_acceptance", func(t *testing.T) {
		ctx, p, h := assignmentFixture(t, backend)
		h.Deadline = time.Now().UTC().Add(time.Minute)
		h.Authority.Budget.Deadline = h.Deadline
		var err error
		h, err = coordination.SealHandoff(h)
		require.NoError(t, err)
		_, working, change, job := submissionFixtureForHandoff(ctx, t, backend, p, h, p.Contract)
		req := finishTaskAcceptanceFixture(ctx, t, backend, working, change, job)
		clock := &jobClock{at: h.Deadline.Add(time.Second)}
		_, err = assurance.New(driver).WithClock(clock).AcceptTaskResult(ctx, req)
		require.Error(t, err, "a longer claim lease must not extend the handoff")
	})
	t.Run("completed_claim_replan", func(t *testing.T) {
		ctx, req := taskAcceptanceFixture(t, backend)
		repo := assurance.New(driver)
		accepted, err := repo.AcceptTaskResult(ctx, req)
		require.NoError(t, err)
		p, err := repo.GetPlan(ctx, req.Claim.PlanID)
		require.NoError(t, err)
		next := coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: req.Claim.TaskID, ExpectedRevision: accepted.Claim.Revision, Worker: req.Claim.Worker, GrantRef: accepted.Claim.GrantRef, Duration: time.Minute}
		_, err = repo.AcquireTaskClaim(ctx, next)
		require.ErrorIs(t, err, coordination.ErrClaimConflict)
		p, err = repo.CommitRevision(ctx, planning.Revision{Plan: p, Actor: "host", Reason: "reverify revised plan"}, p.Revision)
		require.NoError(t, err)
		next.PlanRevision, next.PlanDigest = p.Revision, p.Digest
		claim, err := repo.AcquireTaskClaim(ctx, next)
		require.NoError(t, err)
		require.Equal(t, accepted.Claim.Fence+1, claim.Fence)
		retained, err := repo.GetTaskClaim(ctx, p.ID, claim.TaskID)
		require.NoError(t, err)
		require.Equal(t, claim, retained)
		retry, err := repo.AcceptTaskResult(ctx, req)
		require.NoError(t, err)
		accepted.Fresh = false
		require.Equal(t, accepted, retry)
	})
	t.Run("scope_and_historical_retry", func(t *testing.T) {
		ctx, req := taskAcceptanceFixture(t, backend)
		repo := assurance.New(driver)
		accepted, err := repo.AcceptTaskResult(ctx, req)
		require.NoError(t, err)
		_, err = repo.GetTaskAcceptance(context.Background(), req.ID)
		require.ErrorIs(t, err, cortex.ErrNoScope)
		_, err = repo.GetTaskAcceptance(ctxWithScope("other"), req.ID)
		require.ErrorIs(t, err, coordination.ErrTaskAcceptanceNotFound)
		require.NoError(t, repo.TombstoneEvidence(evidence.WithDeletionActor(ctx, "host"), req.Current.Candidate, "removed"))
		retry, err := repo.AcceptTaskResult(ctx, req)
		require.NoError(t, err)
		accepted.Fresh = false
		require.Equal(t, accepted, retry)
	})
}

func dependentTaskAcceptanceFixture(ctx context.Context, t *testing.T, backend store.Store, root coordination.TaskAcceptanceRequest, taskIndex int, inputs []evidence.Ref) coordination.TaskAcceptanceRequest {
	t.Helper()
	p, err := resultCapability[planning.Store](t, backend).GetPlan(ctx, root.Claim.PlanID)
	require.NoError(t, err)
	parent, err := resultCapability[coordination.AssignmentStore](t, backend).GetAssignment(ctx, root.AssignmentID)
	require.NoError(t, err)
	h := parent.Handoff
	h.ID, h.AssignmentID, h.TaskID = id.New(coordination.PrefixHandoff), id.New(coordination.PrefixAssignment), p.Tasks[taskIndex].ID
	h.Criteria = p.Tasks[taskIndex].Criteria
	h.Authority, err = planning.TaskAuthority(p, h.TaskID)
	require.NoError(t, err)
	h, err = coordination.SealHandoff(h)
	require.NoError(t, err)
	contract, err := planning.TaskContract(p, h.TaskID, id.New(verification.PrefixContract))
	require.NoError(t, err)
	_, working, change, job := submissionFixtureForHandoff(ctx, t, backend, p, h, contract, func(_ *run.Run, binding *verification.Binding) { binding.Evidence = inputs })
	return finishTaskAcceptanceFixture(ctx, t, backend, working, change, job)
}

func testTaskAcceptanceDependencies(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	for _, mode := range []string{"valid", "missing", "unaccepted", "changed_digest", "changed_context", "missing_input", "cancelled_parent", "deleted_parent_evidence", "transitive_missing", "transitive_valid"} {
		t.Run(mode, func(t *testing.T) {
			config := taskContractConfig(t)
			configure := config.Plan
			config.Plan = func(p *planning.Plan) {
				configure(p)
				criterion := p.Contract.Criteria[0]
				criterion.ID = "third"
				p.Contract.Criteria = append(p.Contract.Criteria, criterion)
				third := p.Tasks[1]
				third.ID, third.Output.Name = id.New(planning.PrefixTask), "third"
				third.Criteria, third.DependsOn = []string{"third"}, []id.ID{p.Tasks[1].ID}
				p.Tasks = append(p.Tasks, third)
			}
			ctx, root := taskAcceptanceFixture(t, backend, config)
			s := resultCapability[coordination.TaskAcceptanceStore](t, backend)
			var accepted coordination.TaskAcceptance
			if mode != "unaccepted" {
				var err error
				accepted, err = s.AcceptTaskResult(ctx, root)
				require.NoError(t, err)
			}
			inputs := []evidence.Ref{root.Current.Candidate}
			if mode == "missing_input" {
				inputs = nil
			}
			child := dependentTaskAcceptanceFixture(ctx, t, backend, root, 1, inputs)
			dep := coordination.AcceptedDependency{AcceptanceID: root.ID, Digest: accepted.Digest, Current: root.Current}
			if mode == "unaccepted" {
				dep.Digest = evidence.Digest([]byte("unaccepted"))
			}
			child.Dependencies = []coordination.AcceptedDependency{dep}
			switch mode {
			case "missing":
				child.Dependencies = nil
			case "changed_digest":
				child.Dependencies[0].Digest = evidence.Digest([]byte("changed"))
			case "changed_context":
				child.Dependencies[0].Current.Context.AuthorityRevision = "revoked"
			case "deleted_parent_evidence":
				require.NoError(t, resultCapability[evidence.Storage](t, backend).TombstoneEvidence(evidence.WithDeletionActor(ctx, "host"), root.Current.Candidate, "revoked"))
			case "cancelled_parent":
				journal := resultCapability[coordination.AssignmentStore](t, backend)
				a, err := journal.GetAssignment(ctx, root.AssignmentID)
				require.NoError(t, err)
				_, err = journal.AdvanceAssignment(ctx, assignmentChange(a, coordination.AssignmentCancelled))
				require.NoError(t, err)
			}
			childAcceptance, err := s.AcceptTaskResult(ctx, child)
			if mode == "valid" || mode == "transitive_valid" || mode == "transitive_missing" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				return
			}
			if mode == "transitive_valid" || mode == "transitive_missing" {
				third := dependentTaskAcceptanceFixture(ctx, t, backend, root, 2, []evidence.Ref{child.Current.Candidate})
				third.Dependencies = []coordination.AcceptedDependency{{AcceptanceID: child.ID, Digest: childAcceptance.Digest, Current: child.Current}}
				if mode == "transitive_valid" {
					third.Dependencies = append(third.Dependencies, dep)
				}
				_, err = s.AcceptTaskResult(ctx, third)
				if mode == "transitive_valid" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			}
		})
	}
}
