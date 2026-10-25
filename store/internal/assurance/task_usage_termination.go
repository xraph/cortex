package assurance

import (
	"context"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

// checkTaskUsageDisposition runs inside the plan/run/journal transaction after
// the usage snapshot. A fresh settlement must still match the closed run state.
func (s *Repository) checkTaskUsageDisposition(ctx context.Context, claim coordination.Claim, request coordination.TaskUsageClosureRequest) error {
	if request.Termination == nil && request.Rejection == nil {
		return nil
	}
	if !request.MatchesClaim(claim) {
		return coordination.ErrTaskUsageConflict
	}
	if _, err := s.taskDispatchClaim(ctx, claim); err != nil {
		return err
	}
	reader, ok := s.driver.(SubmissionRunReader)
	if !ok {
		return coordination.ErrAssignmentUnavailable
	}
	current, err := reader.LockSubmissionRun(ctx, claim.RunID)
	if err != nil {
		return err
	}
	if current == nil || current.ID != claim.RunID || current.Scope.Canonical() != claim.Scope.Canonical() || current.AgentID.String() != claim.Worker.Agent || current.Provenance.TaskClaimID != claim.ID.String() || current.CompletedAt == nil {
		return coordination.ErrTaskUsageConflict
	}
	if rejected := request.Rejection; rejected != nil {
		if current.State != run.StateCompleted || current.Revision != rejected.RunRevision || !current.CompletedAt.Equal(rejected.CompletedAt) || current.ResultRef != rejected.ResultRef || current.VerificationStatus != rejected.VerificationStatus {
			return coordination.ErrTaskUsageConflict
		}
		return nil
	}
	terminal := request.Termination
	if current.Revision != terminal.RunRevision || current.State != terminal.State || !current.CompletedAt.Equal(terminal.CompletedAt) || current.ResultRef != "" || current.VerificationStatus == verification.StatusPassed {
		return coordination.ErrTaskUsageConflict
	}
	return nil
}
