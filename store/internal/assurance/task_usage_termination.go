package assurance

import (
	"context"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/verification"
)

// checkTaskUsageTermination runs inside the plan/run/journal transaction after
// the usage snapshot. A fresh settlement must still match the closed run state.
func (s *Repository) checkTaskUsageTermination(ctx context.Context, claim coordination.Claim, request coordination.TaskUsageClosureRequest) error {
	if request.Termination == nil {
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
	terminal := request.Termination
	if current == nil || current.ID != claim.RunID || current.Scope.Canonical() != claim.Scope.Canonical() || current.AgentID.String() != claim.Worker.Agent || current.Provenance.TaskClaimID != claim.ID.String() || current.Revision != terminal.RunRevision || current.State != terminal.State || current.CompletedAt == nil || !current.CompletedAt.Equal(terminal.CompletedAt) || current.ResultRef != "" || current.VerificationStatus == verification.StatusPassed {
		return coordination.ErrTaskUsageConflict
	}
	return nil
}
