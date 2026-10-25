package storetest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

func submissionFixture(t *testing.T, s store.Store, alter ...func(*run.Run, *verification.Binding)) (context.Context, coordination.Assignment, coordination.AssignmentChange, verification.Job) {
	t.Helper()
	return configuredSubmissionFixture(t, s, submissionFixtureConfig{}, alter...)
}

type submissionFixtureConfig struct {
	Plan     func(*planning.Plan)
	Contract func(planning.Plan) verification.Contract
}

func configuredSubmissionFixture(t *testing.T, s store.Store, config submissionFixtureConfig, alter ...func(*run.Run, *verification.Binding)) (context.Context, coordination.Assignment, coordination.AssignmentChange, verification.Job) {
	t.Helper()
	var configure []func(*planning.Plan)
	if config.Plan != nil {
		configure = append(configure, config.Plan)
	}
	ctx, p, h := assignmentFixture(t, s, configure...)
	contract := p.Contract
	if config.Contract != nil {
		contract = config.Contract(p)
	}
	return submissionFixtureForHandoff(ctx, t, s, p, h, contract, alter...)
}

func submissionFixtureForHandoff(ctx context.Context, t *testing.T, s store.Store, p planning.Plan, h coordination.Handoff, contract verification.Contract, alter ...func(*run.Run, *verification.Binding)) (context.Context, coordination.Assignment, coordination.AssignmentChange, verification.Job) {
	t.Helper()
	return submissionFixtureForHandoffBudget(ctx, t, s, p, h, contract, nil, alter...)
}

func submissionFixtureForHandoffBudget(ctx context.Context, t *testing.T, s store.Store, p planning.Plan, h coordination.Handoff, contract verification.Contract, budget *verification.EvaluationBudget, alter ...func(*run.Run, *verification.Binding)) (context.Context, coordination.Assignment, coordination.AssignmentChange, verification.Job) {
	t.Helper()
	journal := resultCapability[coordination.AssignmentStore](t, s)
	current, err := journal.ProposeAssignment(ctx, h)
	require.NoError(t, err)
	current, err = journal.AdvanceAssignment(ctx, assignmentChange(current, coordination.AssignmentAccepted))
	require.NoError(t, err)
	current, err = journal.AdvanceAssignment(ctx, assignmentChange(current, coordination.AssignmentWorking))
	require.NoError(t, err)
	r := mustCreateRun(t, s, ctx)
	r.State, r.StopReason, r.Output = run.StateCompleted, run.StopFinalAnswer, "candidate"
	r.Provenance.PreparedDigest = evidence.Digest([]byte("prepared"))
	r.Provenance.ParentRunID = h.ParentRunID.String()
	r.Provenance.AssignmentDigest = h.Digest
	r.Provenance.AcceptanceRef = contract.ID.String()
	require.NoError(t, s.UpdateRun(ctx, r))
	artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: h.Scope, Version: "1", Producer: planning.ArtifactProducer(p, h.TaskID), MediaType: "text/plain", Sensitivity: evidence.SensitivityRestricted, CreatedAt: time.Now().UTC().Add(-time.Second), RetainUntil: h.Deadline.Add(time.Hour)}, Data: []byte(r.Output)})
	require.NoError(t, err)
	binding := verification.Binding{Scope: h.Scope, RunID: r.ID, ContractDigest: contract.Digest, Candidate: artifact.Ref, Context: verification.ContextSnapshot{PrincipalRef: h.Receiver.Principal, AuthorityRevision: "1", PreparedDigest: r.Provenance.PreparedDigest, PolicyRevision: "1", EnvironmentRevision: "1", DatasetRevision: "1", Sources: p.Sources, Tools: []verification.ToolRevision{{Name: "lookup", Revision: "1"}}}}
	for _, mutate := range alter {
		mutate(r, &binding)
	}
	if len(alter) > 0 {
		require.NoError(t, s.UpdateRun(ctx, r))
	}
	require.NoError(t, resultCapability[verification.Store](t, s).CreateContract(ctx, contract))
	job, err := resultCapability[verification.CaptureStore](t, s).CaptureVerificationJob(ctx, verification.JobRequest{SchemaVersion: 1, ID: id.New(verification.PrefixJob), ExpectedRunRevision: r.Revision, ContractID: contract.ID, Binding: binding, Deadline: h.Deadline, MaxAttempts: 2, EvaluationBudget: budget})
	require.NoError(t, err)
	digest, err := binding.Digest()
	require.NoError(t, err)
	change := assignmentChange(current, coordination.AssignmentSubmitted)
	change.Submission = &coordination.Submission{SchemaVersion: 1, JobID: job.Request.ID, JobRequestDigest: job.RequestDigest, RunID: r.ID, RunRevision: job.RunRevision, BindingDigest: digest}
	return ctx, current, change, job
}

func testAssignmentSubmission(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, working, change, job := submissionFixture(t, s)
	journal := resultCapability[coordination.AssignmentStore](t, s)
	submitted, err := journal.AdvanceAssignment(ctx, change)
	require.NoError(t, err)
	require.Equal(t, coordination.AssignmentSubmitted, submitted.State)
	current, err := s.GetRun(ctx, job.Request.Binding.RunID)
	require.NoError(t, err)
	require.Equal(t, verification.StatusPending, current.VerificationStatus, "submission must not verify the run")
	require.Equal(t, job.RunRevision, current.Revision)
	retry, err := journal.AdvanceAssignment(ctx, change)
	require.NoError(t, err)
	require.Equal(t, submitted, retry)
	changed := change
	changed.Reason = "changed retry"
	_, err = journal.AdvanceAssignment(ctx, changed)
	require.ErrorIs(t, err, coordination.ErrAssignmentConflict)
	require.NoError(t, resultCapability[evidence.Storage](t, s).TombstoneEvidence(evidence.WithDeletionActor(ctx, "host"), job.Request.Binding.Candidate, "removed"))
	retry, err = journal.AdvanceAssignment(ctx, change)
	require.NoError(t, err)
	require.Equal(t, submitted, retry, "historical acknowledgment must not reload deleted bytes")
	cancelled, err := journal.AdvanceAssignment(ctx, assignmentChange(submitted, coordination.AssignmentCancelled))
	require.NoError(t, err)
	require.Equal(t, submitted.Submission, cancelled.Submission)
	history, err := journal.AssignmentHistory(ctx, coordination.AssignmentHistoryRequest{AssignmentID: working.Handoff.AssignmentID, Limit: 10})
	require.NoError(t, err)
	require.Len(t, history, 5)
	require.Equal(t, submitted, history[3])
	require.Equal(t, cancelled, history[4])
}

func testAssignmentSubmissionDenied(t *testing.T, newStore func(*testing.T) store.Store) {
	// One backend per group; fixtures use independent scopes and IDs.
	s := newStore(t)
	for _, mode := range []string{"no evidence", "job digest", "binding", "run revision", "parent", "assignment", "principal", "producer", "media", "tools", "sources", "oversize", "missing job", "output", "run state", "replan", "scope", "cancelled", "parent run"} {
		t.Run(mode, func(t *testing.T) {
			ctx, working, change, job := submissionFixture(t, s, func(r *run.Run, b *verification.Binding) {
				switch mode {
				case "parent":
					r.Provenance.ParentRunID = id.NewAgentRunID().String()
				case "assignment":
					r.Provenance.AssignmentDigest = evidence.Digest([]byte("other handoff"))
				case "principal":
					b.Context.PrincipalRef = "other principal"
				case "producer":
					b.Candidate.Producer = "other task"
				case "media":
					b.Candidate.MediaType = "application/json"
				case "tools":
					b.Context.Tools[0].Revision = "2"
				case "sources":
					b.Context.Sources[0].Revision = "2"
				case "oversize":
					r.Output = strings.Repeat("a", 5000)
					b.Candidate.Bytes = int64(len(r.Output))
					b.Candidate.SHA256 = evidence.Digest([]byte(r.Output))
				}
			})
			journal := resultCapability[coordination.AssignmentStore](t, s)
			r, err := s.GetRun(ctx, job.Request.Binding.RunID)
			require.NoError(t, err)
			switch mode {
			case "no evidence":
				require.NoError(t, resultCapability[evidence.Storage](t, s).TombstoneEvidence(evidence.WithDeletionActor(ctx, "host"), job.Request.Binding.Candidate, "removed"))
			case "job digest":
				change.Submission.JobRequestDigest = evidence.Digest([]byte("other"))
			case "binding":
				change.Submission.BindingDigest = evidence.Digest([]byte("other"))
			case "run revision":
				change.Submission.RunRevision++
			case "output":
				r.Output = "different"
			case "run state":
				r.State = run.StateCancelled
			case "replan":
				plans := resultCapability[planning.Store](t, s)
				p, readErr := plans.GetPlan(ctx, working.Handoff.PlanID)
				require.NoError(t, readErr)
				_, err = plans.CommitRevision(ctx, planning.Revision{Plan: p, Actor: "host", Reason: "superseded"}, 1)
				require.NoError(t, err)
			case "scope":
				ctx = context.Background()
			case "cancelled":
				bounded, cancel := context.WithCancel(ctx)
				cancel()
				ctx = bounded
			case "missing job":
				change.Submission.JobID = id.New(verification.PrefixJob)
			case "parent run":
				change.Submission.RunID = working.Handoff.ParentRunID
			}
			if mode == "output" || mode == "run state" {
				require.NoError(t, s.UpdateRun(ctx, r))
				change.Submission.RunRevision = r.Revision
			}
			result, err := journal.AdvanceAssignment(ctx, change)
			require.Error(t, err)
			require.Zero(t, result)
		})
	}
}

func testAssignmentSubmissionCommitted(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, _, change, job := submissionFixture(t, s)
	jobs := resultCapability[verification.JobStore](t, s)
	job, err := jobs.ClaimVerificationJob(ctx, verification.JobClaim{JobID: job.Request.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "verifier", Duration: time.Minute})
	require.NoError(t, err)
	now := time.Now().UTC()
	report, err := verification.SealReport(verification.Report{SchemaVersion: 1, ID: id.New(verification.PrefixReport), Binding: job.Request.Binding, CheckedAt: now, ExpiresAt: now.Add(time.Minute), Outcomes: []verification.Outcome{{CriterionID: "check", Verifier: "tests", VerifierRevision: "1", Status: verification.StatusPassed}}})
	require.NoError(t, err)
	job, err = jobs.SaveVerificationJobReport(ctx, *job.Lease, report)
	require.NoError(t, err)
	receipt, err := resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, jobCommitRequest(job))
	require.NoError(t, err)
	change.Submission.RunRevision = receipt.RunRevision
	submitted, err := resultCapability[coordination.AssignmentStore](t, s).AdvanceAssignment(ctx, change)
	require.NoError(t, err)
	require.Equal(t, coordination.AssignmentSubmitted, submitted.State)
}

func testAssignmentSubmissionFailedVerification(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	for _, status := range []verification.Status{verification.StatusFailed, verification.StatusInconclusive} {
		t.Run(string(status), func(t *testing.T) {
			ctx, _, change, job := submissionFixture(t, s)
			jobs := resultCapability[verification.JobStore](t, s)
			job, err := jobs.ClaimVerificationJob(ctx, verification.JobClaim{JobID: job.Request.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "verifier", Duration: time.Minute})
			require.NoError(t, err)
			now := time.Now().UTC()
			report, err := verification.SealReport(verification.Report{SchemaVersion: 1, ID: id.New(verification.PrefixReport), Binding: job.Request.Binding, CheckedAt: now, ExpiresAt: now.Add(time.Minute), Outcomes: []verification.Outcome{{CriterionID: "check", Verifier: "tests", VerifierRevision: "1", Status: status}}})
			require.NoError(t, err)
			_, err = jobs.SaveVerificationJobReport(ctx, *job.Lease, report)
			require.NoError(t, err)
			r, err := s.GetRun(ctx, job.Request.Binding.RunID)
			require.NoError(t, err)
			change.Submission.RunRevision = r.Revision
			submitted, err := resultCapability[coordination.AssignmentStore](t, s).AdvanceAssignment(ctx, change)
			require.NoError(t, err)
			require.Equal(t, coordination.AssignmentSubmitted, submitted.State)
			after, err := s.GetRun(ctx, r.ID)
			require.NoError(t, err)
			require.Equal(t, status, after.VerificationStatus)
			require.Empty(t, after.ResultRef)
		})
	}
}
