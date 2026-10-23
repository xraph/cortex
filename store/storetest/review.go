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
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

func storedReviewFixture(t *testing.T, s store.Store, configs ...submissionFixtureConfig) (context.Context, coordination.Review, coordination.Assignment, verification.Job) {
	t.Helper()
	config := submissionFixtureConfig{}
	if len(configs) > 0 {
		config = configs[0]
	}
	ctx, _, change, job := configuredSubmissionFixture(t, s, config)
	journal := resultCapability[coordination.AssignmentStore](t, s)
	subject, err := journal.AdvanceAssignment(ctx, change)
	require.NoError(t, err)
	h := subject.Handoff
	h.ID, h.AssignmentID = id.New(coordination.PrefixHandoff), id.New(coordination.PrefixAssignment)
	h.Purpose = coordination.HandoffReview
	h.Receiver = coordination.Identity{Agent: "reviewer", Principal: "reviewer-1"}
	h.Authority.Resources, h.Authority.Effects = nil, nil
	h.Inputs = append([]evidence.Ref{job.Request.Binding.Candidate}, job.Request.Binding.Evidence...)
	h, err = coordination.SealHandoff(h)
	require.NoError(t, err)
	assignment, err := journal.ProposeAssignment(ctx, h)
	require.NoError(t, err)
	for _, state := range []coordination.AssignmentState{coordination.AssignmentAccepted, coordination.AssignmentWorking} {
		assignment, err = journal.AdvanceAssignment(ctx, assignmentChange(assignment, state))
		require.NoError(t, err)
	}
	reviewer := mustCreateRun(t, s, ctx)
	r := coordination.Review{SchemaVersion: 1, ID: id.New(coordination.PrefixReview), Handoff: h, SubjectAssignmentID: subject.Handoff.AssignmentID, SubjectRevision: subject.Revision, SubjectDigest: subject.Digest, BindingDigest: subject.Submission.BindingDigest, RunID: reviewer.ID, RunRevision: reviewer.Revision + 1, PreparedDigest: evidence.Digest([]byte("review instructions")), ReviewedAt: time.Now().UTC(), Findings: []coordination.ReviewFinding{{CriterionID: h.Criteria[0], Assessment: coordination.ReviewContradicted, Reason: "the candidate does not satisfy the requirement", EvidenceIDs: []id.ID{job.Request.Binding.Candidate.ID}}}}
	r, err = coordination.SealReview(r)
	require.NoError(t, err)
	output, err := r.Output()
	require.NoError(t, err)
	reviewer.State, reviewer.StopReason, reviewer.Output = run.StateCompleted, run.StopFinalAnswer, string(output)
	reviewer.Provenance.PreparedDigest, reviewer.Provenance.AssignmentDigest = r.PreparedDigest, h.Digest
	reviewer.Provenance.ParentRunID, reviewer.Provenance.AcceptanceRef = h.ParentRunID.String(), job.Request.ContractID.String()
	require.NoError(t, s.UpdateRun(ctx, reviewer))
	require.Equal(t, r.RunRevision, reviewer.Revision)
	return ctx, r, subject, job
}

func testAssignmentReviews(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	reviews := resultCapability[coordination.ReviewStore](t, s)
	ctx, r, subject, job := storedReviewFixture(t, s)
	saved, err := reviews.RecordAssignmentReview(ctx, r)
	require.NoError(t, err)
	require.Equal(t, r, saved)
	got, err := reviews.GetAssignmentReview(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, r, got)
	list, err := reviews.ListAssignmentReviews(ctx, coordination.ReviewListRequest{AssignmentID: r.SubjectAssignmentID, Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []coordination.Review{r}, list)
	list, err = reviews.ListAssignmentReviews(ctx, coordination.ReviewListRequest{AssignmentID: r.SubjectAssignmentID, After: r.ID, Limit: 1})
	require.NoError(t, err)
	require.Empty(t, list)
	current, err := resultCapability[coordination.AssignmentStore](t, s).GetAssignment(ctx, r.SubjectAssignmentID)
	require.NoError(t, err)
	require.Equal(t, subject, current)
	producer, err := s.GetRun(ctx, job.Request.Binding.RunID)
	require.NoError(t, err)
	require.Equal(t, verification.StatusPending, producer.VerificationStatus)
	changed := r
	changed.Findings = append([]coordination.ReviewFinding(nil), r.Findings...)
	changed.Findings[0].Reason = "rewritten contradiction"
	changed, err = coordination.SealReview(changed)
	require.NoError(t, err)
	_, err = reviews.RecordAssignmentReview(ctx, changed)
	require.ErrorIs(t, err, coordination.ErrReviewConflict)
	require.NoError(t, resultCapability[evidence.Storage](t, s).TombstoneEvidence(evidence.WithDeletionActor(ctx, "host"), job.Request.Binding.Candidate, "removed"))
	retry, err := reviews.RecordAssignmentReview(ctx, r)
	require.NoError(t, err)
	require.Equal(t, r, retry)
	_, err = reviews.GetAssignmentReview(context.Background(), r.ID)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	other := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
	_, err = reviews.GetAssignmentReview(other, r.ID)
	require.ErrorIs(t, err, coordination.ErrReviewNotFound)
	_, err = reviews.RecordAssignmentReview(other, r)
	require.Error(t, err)
}

func testAssignmentReviewsDenied(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	reviews := resultCapability[coordination.ReviewStore](t, s)
	for _, mode := range []string{"output", "lineage", "revision", "state", "missing evidence", "cancelled review", "cancelled subject", "context", "run identity", "future"} {
		t.Run(mode, func(t *testing.T) {
			ctx, r, subject, job := storedReviewFixture(t, s)
			reviewer, err := s.GetRun(ctx, r.RunID)
			require.NoError(t, err)
			switch mode {
			case "output":
				reviewer.Output = `{"approved":true}`
			case "lineage":
				reviewer.Provenance.AssignmentDigest = evidence.Digest([]byte("other"))
			case "revision":
				r.RunRevision++
			case "state":
				reviewer.State = run.StateFailed
			case "missing evidence":
				require.NoError(t, resultCapability[evidence.Storage](t, s).TombstoneEvidence(evidence.WithDeletionActor(ctx, "host"), job.Request.Binding.Candidate, "removed"))
			case "cancelled review":
				a, readErr := resultCapability[coordination.AssignmentStore](t, s).GetAssignment(ctx, r.Handoff.AssignmentID)
				require.NoError(t, readErr)
				_, err = resultCapability[coordination.AssignmentStore](t, s).AdvanceAssignment(ctx, assignmentChange(a, coordination.AssignmentCancelled))
				require.NoError(t, err)
			case "cancelled subject":
				_, err = resultCapability[coordination.AssignmentStore](t, s).AdvanceAssignment(ctx, assignmentChange(subject, coordination.AssignmentCancelled))
				require.NoError(t, err)
			case "context":
				r.PreparedDigest = job.Request.Binding.Context.PreparedDigest
			case "run identity":
				r.RunID = job.Request.Binding.RunID
			case "future":
				r.ReviewedAt = time.Now().UTC().Add(time.Minute)
			}
			if mode == "output" || mode == "lineage" || mode == "state" {
				require.NoError(t, s.UpdateRun(ctx, reviewer))
				r.RunRevision = reviewer.Revision
			}
			r, err = coordination.SealReview(r)
			require.NoError(t, err)
			_, err = reviews.RecordAssignmentReview(ctx, r)
			require.Error(t, err)
			_, err = reviews.GetAssignmentReview(ctx, r.ID)
			require.ErrorIs(t, err, coordination.ErrReviewNotFound)
		})
	}
}

func testAssignmentReviewAfterVerification(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	for _, projected := range []bool{false, true} {
		t.Run(map[bool]string{false: "plan_contract", true: "task_contract"}[projected], func(t *testing.T) {
			config := submissionFixtureConfig{}
			if projected {
				config = taskContractConfig(t)
			}
			for _, status := range []verification.Status{verification.StatusFailed, verification.StatusInconclusive, verification.StatusPassed} {
				t.Run(string(status), func(t *testing.T) {
					ctx, r, _, job := storedReviewFixture(t, s, config)
					jobs := resultCapability[verification.JobStore](t, s)
					job, err := jobs.ClaimVerificationJob(ctx, verification.JobClaim{JobID: job.Request.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "verifier", Duration: time.Minute})
					require.NoError(t, err)
					now := time.Now().UTC()
					report, err := verification.SealReport(verification.Report{SchemaVersion: 1, ID: id.New(verification.PrefixReport), Binding: job.Request.Binding, CheckedAt: now, ExpiresAt: now.Add(time.Minute), Outcomes: []verification.Outcome{{CriterionID: "check", Verifier: "tests", VerifierRevision: "1", Status: status}}})
					require.NoError(t, err)
					job, err = jobs.SaveVerificationJobReport(ctx, *job.Lease, report)
					require.NoError(t, err)
					if status == verification.StatusPassed {
						_, err = resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, jobCommitRequest(job))
						require.NoError(t, err)
					}
					before, err := s.GetRun(ctx, job.Request.Binding.RunID)
					require.NoError(t, err)
					_, err = resultCapability[coordination.ReviewStore](t, s).RecordAssignmentReview(ctx, r)
					require.NoError(t, err)
					after, err := s.GetRun(ctx, before.ID)
					require.NoError(t, err)
					require.Equal(t, before, after, "review must retain the current verifier verdict")
				})
			}
		})
	}
}

func testAssignmentReviewPrivateOutput(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	for _, projected := range []bool{false, true} {
		t.Run(map[bool]string{false: "plan_contract", true: "task_contract"}[projected], func(t *testing.T) {
			config := submissionFixtureConfig{}
			if projected {
				config = taskContractConfig(t)
			}
			for _, deleted := range []bool{false, true} {
				ctx, r, _, producerJob := storedReviewFixture(t, s, config)
				current, err := s.GetRun(ctx, r.RunID)
				require.NoError(t, err)
				output := current.Output
				current.State, current.StopReason, current.Output = run.StateRunning, "", ""
				require.NoError(t, s.UpdateRun(ctx, current))
				binding := producerJob.Request.Binding
				binding.RunID = r.RunID
				binding.Context.PreparedDigest, binding.Context.PrincipalRef = r.PreparedDigest, r.Handoff.Receiver.Principal
				binding.Candidate.ID = id.New(evidence.PrefixArtifact)
				binding.Candidate.Producer = r.Handoff.Digest
				binding.Candidate.MediaType = "application/json"
				binding.Candidate.Bytes, binding.Candidate.SHA256 = 0, evidence.Digest(nil)
				template := verification.JobRequest{SchemaVersion: 1, ID: id.New(verification.PrefixJob), ExpectedRunRevision: current.Revision, ContractID: producerJob.Request.ContractID, Binding: binding, Deadline: r.Handoff.Deadline, MaxAttempts: 2, EvaluationBudget: &verification.EvaluationBudget{MaxCalls: 2, MaxTokens: 100, TokenOnly: true}}
				intent, err := verification.SealCaptureIntent(verification.CaptureIntent{SchemaVersion: 1, ID: id.New(verification.PrefixCaptureIntent), Template: template, ResultID: id.New(verification.PrefixResult), MaxCandidateBytes: coordination.MaxReviewBytes})
				require.NoError(t, err)
				require.NoError(t, resultCapability[verification.CaptureIntentStore](t, s).CreateCaptureIntent(ctx, intent))
				job, err := resultCapability[run.CandidateStore](t, s).CaptureCandidate(ctx, run.CandidateCapture{SchemaVersion: 1, IntentID: intent.ID, ExpectedRunRevision: current.Revision, Output: output, FinishReason: "stop"})
				require.NoError(t, err)
				r.RunRevision = job.RunRevision
				r, err = coordination.SealReview(r)
				require.NoError(t, err)
				if deleted {
					require.NoError(t, resultCapability[evidence.Storage](t, s).TombstoneEvidence(evidence.WithDeletionActor(ctx, "host"), job.Request.Binding.Candidate, "removed"))
				}
				_, err = resultCapability[coordination.ReviewStore](t, s).RecordAssignmentReview(ctx, r)
				if deleted {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
				after, readErr := s.GetRun(ctx, r.RunID)
				require.NoError(t, readErr)
				require.Empty(t, after.Output)
			}
		})
	}
}
