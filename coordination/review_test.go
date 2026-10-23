package coordination_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/verification"
)

func reviewFixture(t *testing.T) (coordination.Review, coordination.Assignment, verification.Binding, planning.Plan, time.Time) {
	t.Helper()
	h, p, _, now := handoffFixture(t)
	h, err := coordination.SealHandoff(h)
	require.NoError(t, err)
	a, err := coordination.Propose(h, now)
	require.NoError(t, err)
	for _, state := range []coordination.AssignmentState{coordination.AssignmentAccepted, coordination.AssignmentWorking} {
		a, err = coordination.Advance(a, coordination.AssignmentChange{AssignmentID: h.AssignmentID, ExpectedRevision: a.Revision, HandoffDigest: h.Digest, Fence: h.Fence, State: state, Actor: h.Receiver.Principal, Reason: "work"}, now)
		require.NoError(t, err)
	}
	artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: h.Scope, Version: "1", Producer: planning.ArtifactProducer(p, h.TaskID), MediaType: "text/plain", Sensitivity: evidence.SensitivityRestricted, CreatedAt: now.Add(-time.Second), RetainUntil: h.Deadline.Add(time.Hour)}, Data: []byte("candidate")})
	require.NoError(t, err)
	binding := verification.Binding{Scope: h.Scope, RunID: id.NewAgentRunID(), ContractDigest: h.ContractDigest, Candidate: artifact.Ref, Context: verification.ContextSnapshot{PrincipalRef: h.Receiver.Principal, PreparedDigest: evidence.Digest([]byte("producer")), AuthorityRevision: "1", PolicyRevision: "1", EnvironmentRevision: "1", DatasetRevision: "1"}}
	digest, err := binding.Digest()
	require.NoError(t, err)
	a, err = coordination.Advance(a, coordination.AssignmentChange{AssignmentID: h.AssignmentID, ExpectedRevision: a.Revision, HandoffDigest: h.Digest, Fence: h.Fence, State: coordination.AssignmentSubmitted, Actor: h.Receiver.Principal, Reason: "submitted", Submission: &coordination.Submission{SchemaVersion: 1, JobID: id.New(verification.PrefixJob), JobRequestDigest: evidence.Digest([]byte("job")), RunID: binding.RunID, RunRevision: 2, BindingDigest: digest}}, now)
	require.NoError(t, err)
	reviewHandoff := h
	reviewHandoff.ID, reviewHandoff.AssignmentID = id.New(coordination.PrefixHandoff), id.New(coordination.PrefixAssignment)
	reviewHandoff.Purpose = coordination.HandoffReview
	reviewHandoff.Receiver = coordination.Identity{Agent: "reviewer", Principal: "reviewer"}
	reviewHandoff.Authority.Resources, reviewHandoff.Authority.Effects = nil, nil
	reviewHandoff.Inputs = []evidence.Ref{artifact.Ref}
	reviewHandoff, err = coordination.SealHandoff(reviewHandoff)
	require.NoError(t, err)
	r, err := coordination.SealReview(coordination.Review{SchemaVersion: 1, ID: id.New(coordination.PrefixReview), Handoff: reviewHandoff, SubjectAssignmentID: h.AssignmentID, SubjectRevision: a.Revision, SubjectDigest: a.Digest, BindingDigest: digest, RunID: id.NewAgentRunID(), RunRevision: 1, PreparedDigest: evidence.Digest([]byte("independent review instructions")), ReviewedAt: now, Findings: []coordination.ReviewFinding{{CriterionID: "checked", Assessment: coordination.ReviewContradicted, Reason: "candidate contradicts the requirement", EvidenceIDs: []id.ID{artifact.Ref.ID}}}})
	require.NoError(t, err)
	return r, a, binding, p, now
}

func TestIndependentReviewAdmission(t *testing.T) {
	r, a, b, p, now := reviewFixture(t)
	require.NoError(t, coordination.ValidateReview(r, a, b, p, now))
	data, err := r.Output()
	require.NoError(t, err)
	require.NoError(t, r.ValidateOutput(data))
	require.Error(t, r.ValidateOutput(append(data, []byte(" {}")...)))
	require.Error(t, r.ValidateOutput([]byte(`{"approved":true}`)))
	original := r.Findings[0].Reason
	frozen, err := coordination.SealReview(r)
	require.NoError(t, err)
	r.Findings[0].Reason = "mutated"
	require.Equal(t, original, frozen.Findings[0].Reason)
	require.NoError(t, frozen.Validate())
}

func TestReviewRejectsSelfApprovalAndUnboundFindings(t *testing.T) {
	for _, mode := range []string{"principal", "endpoint", "context", "run", "subject", "binding", "plan", "expiry", "future", "missing candidate", "unknown evidence", "missing criterion", "duplicate criterion", "effect", "work purpose", "success"} {
		t.Run(mode, func(t *testing.T) {
			r, a, b, p, now := reviewFixture(t)
			switch mode {
			case "principal":
				r.Handoff.Receiver.Principal = a.Handoff.Receiver.Principal
			case "endpoint":
				r.Handoff.Receiver.Agent = a.Handoff.Receiver.Agent
				r.Handoff.Receiver.Node = a.Handoff.Receiver.Node
			case "context":
				r.PreparedDigest = b.Context.PreparedDigest
			case "run":
				r.RunID = b.RunID
			case "subject":
				r.SubjectRevision++
			case "binding":
				r.BindingDigest = evidence.Digest([]byte("other"))
			case "plan":
				r.Handoff.PlanRevision++
			case "expiry":
				now = r.Handoff.Deadline
			case "future":
				r.ReviewedAt = now.Add(time.Second)
			case "missing candidate":
				r.Handoff.Inputs = nil
				r.Findings[0].Assessment = coordination.ReviewMissing
				r.Findings[0].EvidenceIDs = nil
			case "unknown evidence":
				r.Findings[0].EvidenceIDs = []id.ID{id.New(evidence.PrefixArtifact)}
			case "missing criterion":
				r.Findings = nil
			case "duplicate criterion":
				r.Findings = append(r.Findings, r.Findings[0])
			case "effect":
				r.Handoff.Authority.Effects = a.Handoff.Authority.Effects
			case "work purpose":
				r.Handoff.Purpose = coordination.HandoffWork
			case "success":
				r.Findings[0].Assessment = "passed"
			}
			h, err := coordination.SealHandoff(r.Handoff)
			if err != nil {
				return
			}
			r.Handoff = h
			r, err = coordination.SealReview(r)
			if err != nil {
				return
			}
			require.Error(t, coordination.ValidateReview(r, a, b, p, now))
		})
	}
}
