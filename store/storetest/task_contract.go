package storetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

func taskContractConfig(t *testing.T) submissionFixtureConfig {
	t.Helper()
	return submissionFixtureConfig{
		Plan: func(p *planning.Plan) {
			criterion := p.Contract.Criteria[0]
			criterion.ID = "second"
			p.Contract.Criteria = append(p.Contract.Criteria, criterion)
			child := p.Tasks[0]
			child.ID, child.Output.Name, child.Instruction = id.New(planning.PrefixTask), "second", "produce second output"
			child.Criteria, child.DependsOn = []string{"second"}, []id.ID{p.Tasks[0].ID}
			p.Tasks = append(p.Tasks, child)
		},
		Contract: func(p planning.Plan) verification.Contract {
			contract, err := planning.TaskContract(p, p.Tasks[0].ID, id.New(verification.PrefixContract))
			require.NoError(t, err)
			return contract
		},
	}
}

func testTaskContractSubmission(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	for _, afterCommit := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_commit", true: "after_commit"}[afterCommit], func(t *testing.T) {
			ctx, working, change, job := configuredSubmissionFixture(t, s, taskContractConfig(t))
			journal := resultCapability[coordination.AssignmentStore](t, s)
			if !afterCommit {
				_, err := journal.AdvanceAssignment(ctx, change)
				require.NoError(t, err)
			}
			jobs := resultCapability[verification.JobStore](t, s)
			job, err := jobs.ClaimVerificationJob(ctx, verification.JobClaim{JobID: job.Request.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "verifier", Duration: time.Minute})
			require.NoError(t, err)
			now := time.Now().UTC()
			report, err := verification.SealReport(verification.Report{SchemaVersion: 1, ID: id.New(verification.PrefixReport), Binding: job.Request.Binding, CheckedAt: now, ExpiresAt: now.Add(time.Minute), Outcomes: []verification.Outcome{{CriterionID: "check", Verifier: "tests", VerifierRevision: "1", Status: verification.StatusPassed}}})
			require.NoError(t, err)
			job, err = jobs.SaveVerificationJobReport(ctx, *job.Lease, report)
			require.NoError(t, err)
			require.Equal(t, verification.JobReady, job.State, "another task's required checks must not block this output")
			receipt, err := resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, jobCommitRequest(job))
			require.NoError(t, err)
			if afterCommit {
				change.Submission.RunRevision = receipt.RunRevision
				_, err = journal.AdvanceAssignment(ctx, change)
				require.NoError(t, err)
			}
			p, err := resultCapability[planning.Store](t, s).GetPlan(ctx, working.Handoff.PlanID)
			require.NoError(t, err)
			contract, err := resultCapability[verification.Store](t, s).GetContract(ctx, job.Request.ContractID)
			require.NoError(t, err)
			ready, err := planning.Ready(p, []planning.TaskState{{TaskID: working.Handoff.TaskID, PlanDigest: p.Digest, Status: planning.StatusSucceeded, Current: job.Request.Binding, Contract: &contract, Reports: []verification.Report{report}}}, time.Now().UTC())
			require.NoError(t, err)
			require.Equal(t, []id.ID{p.Tasks[1].ID}, ready)
			require.Len(t, p.Contract.Criteria, 2, "task projection must not edit plan requirements")
			require.NotEqual(t, verification.StatusPassed, verification.Aggregate(p.Contract, []verification.Report{report}, now).Status)
		})
	}
}

func testTaskContractSubmissionDenied(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	for _, mode := range []string{"verifier", "manifest", "freshness", "constraints", "other_task", "old_plan"} {
		t.Run(mode, func(t *testing.T) {
			config := taskContractConfig(t)
			config.Contract = func(p planning.Plan) verification.Contract {
				taskID := p.Tasks[0].ID
				if mode == "other_task" {
					taskID = p.Tasks[1].ID
				}
				contract, err := planning.TaskContract(p, taskID, id.New(verification.PrefixContract))
				require.NoError(t, err)
				switch mode {
				case "verifier":
					contract.Criteria[0].VerifierRevision = "other"
				case "manifest":
					contract.Criteria[0].Parameters.ManifestRevision = "other"
				case "freshness":
					contract.Criteria[0].MaxAge += time.Hour
				case "constraints":
					contract.Constraints = []string{"invented constraint"}
				}
				contract, err = verification.SealContract(contract)
				require.NoError(t, err)
				return contract
			}
			ctx, working, change, _ := configuredSubmissionFixture(t, s, config)
			if mode == "old_plan" {
				plans := resultCapability[planning.Store](t, s)
				p, err := plans.GetPlan(ctx, working.Handoff.PlanID)
				require.NoError(t, err)
				_, err = plans.CommitRevision(ctx, planning.Revision{Plan: p, Actor: "host", Reason: "changed"}, p.Revision)
				require.NoError(t, err)
			}
			_, err := resultCapability[coordination.AssignmentStore](t, s).AdvanceAssignment(ctx, change)
			require.Error(t, err)
		})
	}
}

func testTaskContractReviews(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	for _, mode := range []string{"valid", "plan_contract", "other_task", "weakened"} {
		t.Run(mode, func(t *testing.T) {
			ctx, review, subject, job := storedReviewFixture(t, s, taskContractConfig(t))
			if mode != "valid" {
				p, err := resultCapability[planning.Store](t, s).GetPlan(ctx, subject.Handoff.PlanID)
				require.NoError(t, err)
				taskID := subject.Handoff.TaskID
				if mode == "other_task" {
					taskID = p.Tasks[1].ID
				}
				contract, err := planning.TaskContract(p, taskID, id.New(verification.PrefixContract))
				require.NoError(t, err)
				if mode == "plan_contract" {
					contract = p.Contract
				}
				if mode == "weakened" {
					contract.Criteria[0].MaxAge += time.Hour
					contract, err = verification.SealContract(contract)
					require.NoError(t, err)
				}
				if mode != "plan_contract" {
					require.NoError(t, resultCapability[verification.Store](t, s).CreateContract(ctx, contract))
				}
				r, err := s.GetRun(ctx, review.RunID)
				require.NoError(t, err)
				r.Provenance.AcceptanceRef = contract.ID.String()
				require.NoError(t, s.UpdateRun(ctx, r))
				review.RunRevision = r.Revision
				review, err = coordination.SealReview(review)
				require.NoError(t, err)
			}
			_, err := resultCapability[coordination.ReviewStore](t, s).RecordAssignmentReview(ctx, review)
			if mode == "valid" || mode == "plan_contract" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			current, err := s.GetRun(ctx, job.Request.Binding.RunID)
			require.NoError(t, err)
			require.Equal(t, verification.StatusPending, current.VerificationStatus, "review remains advisory")
		})
	}
}
