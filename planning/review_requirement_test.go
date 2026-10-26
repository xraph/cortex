package planning

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReviewRequirementRequiresExplicitPlanVersion(t *testing.T) {
	p := fixturePlan(t)
	original, err := json.Marshal(p)
	require.NoError(t, err)
	require.NotContains(t, string(original), "review")
	p.Tasks[0].Review = &ReviewRequirement{ReviewerAgent: "reviewer", ReviewerPrincipal: "reviewer-principal", PolicyRevision: "review-policy/1"}
	_, err = Seal(p, Limits{})
	require.Error(t, err)
	p.SchemaVersion = 2
	sealed, err := Seal(p, Limits{})
	require.NoError(t, err)
	p.Tasks[0].Review.PolicyRevision = "changed"
	require.Equal(t, "review-policy/1", sealed.Tasks[0].Review.PolicyRevision)
	require.NoError(t, Validate(sealed, Limits{}))
	for _, mutate := range []func(*ReviewRequirement){func(r *ReviewRequirement) { r.ReviewerAgent = "" }, func(r *ReviewRequirement) { r.ReviewerPrincipal = "" }, func(r *ReviewRequirement) { r.PolicyRevision = "" }} {
		p = fixturePlan(t)
		p.SchemaVersion = 2
		p.Tasks[0].Review = &ReviewRequirement{ReviewerAgent: "reviewer", ReviewerPrincipal: "principal", PolicyRevision: "1"}
		mutate(p.Tasks[0].Review)
		_, err = Seal(p, Limits{})
		require.Error(t, err)
	}
}

func TestRevisionCannotRemoveOrMoveRequiredReview(t *testing.T) {
	p := fixturePlan(t)
	p.SchemaVersion = 2
	p.Tasks[0].Review = &ReviewRequirement{ReviewerAgent: "reviewer", ReviewerPrincipal: "principal", PolicyRevision: "1"}
	p, err := Seal(p, Limits{})
	require.NoError(t, err)
	for _, mode := range []string{"drop", "change", "remove_task", "criteria"} {
		t.Run(mode, func(t *testing.T) {
			next, sealErr := Seal(p, Limits{})
			require.NoError(t, sealErr)
			switch mode {
			case "drop":
				next.Tasks[0].Review = nil
				next.SchemaVersion = 1
			case "change":
				next.Tasks[0].Review.PolicyRevision = "2"
			case "remove_task":
				next.Tasks = nil
			case "criteria":
				next.Tasks[0].Criteria = []string{"other"}
			}
			_, revisionErr := ApplyRevision(p, Revision{Plan: next, Actor: "host", Reason: "replan"}, p.Revision, Limits{})
			require.Error(t, revisionErr)
		})
	}
}
