package coordination_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

func TestTaskAdjudicationRequiresIndependentSupportedReview(t *testing.T) {
	r, a, b, p, now := reviewFixture(t)
	p.SchemaVersion = 2
	p.Tasks[0].Review = &planning.ReviewRequirement{ReviewerAgent: r.Handoff.Receiver.Agent, ReviewerPrincipal: r.Handoff.Receiver.Principal, PolicyRevision: "policy/1"}
	// Bind both handoffs and the submission to the new immutable plan.
	p, err := planning.Seal(p, planning.Limits{})
	require.NoError(t, err)
	a.Handoff.PlanDigest = p.Digest
	a.Handoff, err = coordination.SealHandoff(a.Handoff)
	require.NoError(t, err)
	// The pure policy check consumes the retained review, not reconstructed assignment history.
	r.Handoff.PlanDigest = p.Digest
	r.Handoff, err = coordination.SealHandoff(r.Handoff)
	require.NoError(t, err)
	r, err = coordination.SealReview(r)
	require.NoError(t, err)
	req := coordination.TaskAdjudicationRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixTaskAdjudication), PlanID: p.ID, PlanDigest: p.Digest, TaskID: a.Handoff.TaskID, AssignmentID: r.SubjectAssignmentID, AssignmentDigest: r.SubjectDigest, AssignmentRevision: r.SubjectRevision, ReviewID: r.ID, ReviewDigest: r.Digest, BindingDigest: r.BindingDigest, PolicyRevision: "policy/1", Actor: "coordinator", Reason: "independent review"}
	adjudication, err := coordination.PrepareTaskAdjudication(req, r, p, now)
	require.NoError(t, err)
	require.Equal(t, coordination.AdjudicationBlocked, adjudication.Decision)
	r.Findings[0].Assessment = coordination.ReviewSupported
	r, err = coordination.SealReview(r)
	require.NoError(t, err)
	req.ReviewDigest = r.Digest
	adjudication, err = coordination.PrepareTaskAdjudication(req, r, p, now)
	require.NoError(t, err)
	require.Equal(t, coordination.AdjudicationSupported, adjudication.Decision)
	require.NoError(t, adjudication.Validate())
	req.PolicyRevision = "changed"
	_, err = coordination.PrepareTaskAdjudication(req, r, p, now)
	require.Error(t, err)
	_ = b
}

func TestTaskAcceptanceReviewSchemaCannotBeDropped(t *testing.T) {
	a := taskAcceptanceFixture(t)
	a.Request.Adjudication = &coordination.TaskAdjudicationRef{ID: id.New(coordination.PrefixTaskAdjudication), Digest: a.Request.AssignmentDigest}
	_, _, err := a.Request.Freeze()
	require.Error(t, err)
	a.Request.SchemaVersion = 2
	_, _, err = a.Request.Freeze()
	require.NoError(t, err)
	a.Request.Adjudication = nil
	_, _, err = a.Request.Freeze()
	require.Error(t, err)
}
