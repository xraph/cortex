package storetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

// CurrentTaskAcceptanceRead checks operator projections against the same live
// evidence rules used by dependent task admission, including later withdrawal.
func CurrentTaskAcceptanceRead(t *testing.T, s store.Store) {
	for _, mode := range []string{"supported", "source_changed", "contradiction", "cancelled", "extra_dependency"} {
		t.Run(mode, func(t *testing.T) {
			ctx, request, review, adjudicationRequest := taskAdjudicationFixture(t, s)
			reviews := resultCapability[coordination.ReviewStore](t, s)
			_, err := reviews.RecordAssignmentReview(ctx, review)
			require.NoError(t, err)
			adjudication, err := resultCapability[coordination.TaskAdjudicationStore](t, s).RecordTaskAdjudication(ctx, adjudicationRequest)
			require.NoError(t, err)
			request.SchemaVersion = 2
			request.Adjudication = &coordination.TaskAdjudicationRef{ID: adjudication.Request.ID, Digest: adjudication.Digest}
			accepted, err := resultCapability[coordination.TaskAcceptanceStore](t, s).AcceptTaskResult(ctx, request)
			require.NoError(t, err)
			read := coordination.CurrentTaskAcceptanceRequest{Accepted: coordination.AcceptedDependency{AcceptanceID: accepted.Request.ID, Digest: accepted.Digest, Current: request.Current}}
			switch mode {
			case "source_changed":
				read.Accepted.Current.Context.AuthorityRevision = "changed"
			case "extra_dependency":
				read.Dependencies = []coordination.AcceptedDependency{read.Accepted}
			case "cancelled":
				assignments := resultCapability[coordination.AssignmentStore](t, s)
				current, readErr := assignments.GetAssignment(ctx, request.AssignmentID)
				require.NoError(t, readErr)
				_, err = assignments.AdvanceAssignment(ctx, assignmentChange(current, coordination.AssignmentCancelled))
				require.NoError(t, err)
			case "contradiction":
				review.ID = id.New(coordination.PrefixReview)
				review.Findings[0].Assessment = coordination.ReviewContradicted
				current, readErr := s.GetRun(ctx, review.RunID)
				require.NoError(t, readErr)
				output, outputErr := review.Output()
				require.NoError(t, outputErr)
				current.Output = string(output)
				require.NoError(t, s.UpdateRun(ctx, current))
				review.RunRevision = current.Revision
				review, err = coordination.SealReview(review)
				require.NoError(t, err)
				_, err = reviews.RecordAssignmentReview(ctx, review)
				require.NoError(t, err)
			}
			result, err := resultCapability[coordination.CurrentTaskAcceptanceStore](t, s).ReadCurrentTaskAcceptance(ctx, read)
			if mode != "supported" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, accepted.Claim.TaskID, result.State.TaskID)
			require.True(t, time.Now().Before(result.ValidUntil))
		})
	}
}
