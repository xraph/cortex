package assurance

import (
	"context"

	"github.com/xraph/cortex/coordination"
)

var _ coordination.CurrentTaskAcceptanceStore = (*Repository)(nil)

// ReadCurrentTaskAcceptance rechecks current stored evidence without mutating
// ownership, settling usage or generating a new acceptance acknowledgment.
func (s *Repository) ReadCurrentTaskAcceptance(ctx context.Context, request coordination.CurrentTaskAcceptanceRequest) (coordination.CurrentTaskAcceptance, error) {
	if err := request.Validate(); err != nil {
		return coordination.CurrentTaskAcceptance{}, err
	}
	if err := require(ctx, request.Accepted.Current.Scope); err != nil {
		return coordination.CurrentTaskAcceptance{}, err
	}
	accepted, err := s.GetTaskAcceptance(ctx, request.Accepted.AcceptanceID)
	if err != nil {
		return coordination.CurrentTaskAcceptance{}, err
	}
	var result coordination.CurrentTaskAcceptance
	err = s.planTransaction(ctx, accepted.Claim.PlanID, func(attempt context.Context, tx *Repository) error {
		current, readErr := tx.GetTaskAcceptance(attempt, request.Accepted.AcceptanceID)
		if readErr != nil {
			return readErr
		}
		if current.Digest != request.Accepted.Digest || current.Digest != accepted.Digest {
			return coordination.ErrTaskAcceptanceConflict
		}
		plan, readErr := tx.GetPlan(attempt, current.Claim.PlanID)
		if readErr != nil {
			return readErr
		}
		if plan.Digest != current.Claim.PlanDigest {
			return coordination.ErrTaskAcceptanceConflict
		}
		claim, readErr := tx.GetTaskClaim(attempt, plan.ID, current.Claim.TaskID)
		if readErr != nil {
			return readErr
		}
		if claim.Digest != current.Claim.Digest {
			return coordination.ErrTaskAcceptanceConflict
		}
		original, readErr := tx.claimRevision(attempt, plan.ID, claim.TaskID, current.Request.Claim.Revision)
		if readErr != nil {
			return readErr
		}
		if coordination.ValidateClaimSuccessor(original, claim) != nil {
			return coordination.ErrInvalidTaskAcceptance
		}
		input := current.Request
		input.Current = request.Accepted.Current
		input.Dependencies = request.Dependencies
		own, used, readErr := tx.loadTaskAcceptanceMaterial(attempt, plan, input, original, 8<<20, current.ObservedAt)
		if readErr != nil {
			return readErr
		}
		dependencies, readErr := tx.acceptedPrerequisiteMaterials(attempt, plan, claim.TaskID, input.Dependencies, (32<<20)-used)
		if readErr != nil {
			return readErr
		}
		materials := append([]taskAcceptanceMaterial{own}, dependencies...)
		now := s.clock.Now()
		if checkErr := validateTaskAcceptanceMaterials(plan, claim.TaskID, materials, now); checkErr != nil {
			return checkErr
		}
		deadline := own.deadline
		for _, material := range dependencies {
			deadline = minAcceptanceDeadline(deadline, material.deadline)
		}
		result = coordination.CurrentTaskAcceptance{State: own.state, ObservedAt: now, ValidUntil: deadline}
		return nil
	})
	if err != nil {
		return coordination.CurrentTaskAcceptance{}, err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return coordination.CurrentTaskAcceptance{}, contextErr
	}
	if !s.clock.Now().Before(result.ValidUntil) {
		return coordination.CurrentTaskAcceptance{}, coordination.ErrInvalidTaskAcceptance
	}
	return result, nil
}
