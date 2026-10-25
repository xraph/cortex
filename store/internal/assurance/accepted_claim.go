package assurance

import (
	"context"

	"github.com/xraph/cortex/coordination"
)

var _ coordination.AcceptedClaimStore = (*Repository)(nil)

// ListAcceptedTaskClaims preserves accounting discovery after ownership changes.
// Pagination follows acceptance IDs, so successive revisions of one task each
// occupy their own position. No returned claim authorizes execution or settlement.
func (s *Repository) ListAcceptedTaskClaims(ctx context.Context, request coordination.TaskAcceptanceListRequest) ([]coordination.Claim, error) {
	accepted, err := s.ListTaskAcceptances(ctx, request)
	if err != nil {
		return nil, err
	}
	claims := make([]coordination.Claim, 0, len(accepted))
	for _, receipt := range accepted {
		claim, _, readErr := s.acceptedTaskClaim(ctx, receipt.Claim.Token())
		if readErr != nil {
			return nil, readErr
		}
		if claim.Digest != receipt.Claim.Digest {
			return nil, coordination.ErrClaimConflict
		}
		claims = append(claims, claim)
	}
	return claims, ctx.Err()
}

// acceptedTaskClaim verifies immutable accepted ownership without treating it as
// current execution authority. Accounting callers hold the plan guard through
// their write and authenticate the exact historical run under current host policy.
func (s *Repository) acceptedTaskClaim(ctx context.Context, token coordination.ClaimToken) (coordination.Claim, coordination.TaskAcceptance, error) {
	var empty coordination.TaskAcceptance
	if token.Validate() != nil {
		return coordination.Claim{}, empty, coordination.ErrInvalidClaim
	}
	claim, err := s.claimRevision(ctx, token.PlanID, token.TaskID, token.Revision)
	if err != nil {
		return coordination.Claim{}, empty, err
	}
	if claim.Token() != token || claim.State != coordination.ClaimCompleted || claim.AcceptanceID == nil || claim.Revision < 2 {
		return coordination.Claim{}, empty, coordination.ErrClaimConflict
	}
	previous, err := s.claimRevision(ctx, claim.PlanID, claim.TaskID, claim.Revision-1)
	if err != nil {
		return coordination.Claim{}, empty, err
	}
	if coordination.ValidateClaimSuccessor(previous, claim) != nil {
		return coordination.Claim{}, empty, coordination.ErrInvalidClaim
	}
	accepted, err := s.GetTaskAcceptance(ctx, *claim.AcceptanceID)
	if err != nil {
		return coordination.Claim{}, empty, err
	}
	if accepted.Claim.Digest != claim.Digest {
		return coordination.Claim{}, empty, coordination.ErrClaimConflict
	}
	if _, err = s.taskDispatchClaim(ctx, claim); err != nil {
		return coordination.Claim{}, empty, err
	}
	return claim, accepted, nil
}
