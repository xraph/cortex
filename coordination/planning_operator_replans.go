package coordination

import (
	"context"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

// PlanningReplanView exposes a retained proposal for explicit operator review.
// Proposal text is model output and never grants new resource authority.
type PlanningReplanView struct {
	ID               id.ID              `json:"id"`
	RunID            id.AgentRunID      `json:"run_id"`
	BaseRevision     int64              `json:"base_revision"`
	BaseDigest       string             `json:"base_digest"`
	Status           string             `json:"status"`
	ProposedDigest   string             `json:"proposed_digest,omitempty"`
	ProposedTasks    []PlanningTaskView `json:"proposed_tasks,omitempty"`
	AcceptedRevision int64              `json:"accepted_revision,omitempty"`
}

func (o *PlanningOperator) replanViews(ctx context.Context, plan planning.Plan) ([]PlanningReplanView, error) {
	attempts, err := o.config.Store.ListPlanReplans(ctx, plan.ID)
	if err != nil {
		return nil, err
	}
	if len(attempts) > planning.MaxReplanAttempts {
		return nil, planning.ErrConflict
	}
	views := make([]PlanningReplanView, 0, len(attempts))
	for _, attempt := range attempts {
		p := attempt.Request.Progress.Plan
		if attempt.Validate() != nil || p.ID != plan.ID || p.Scope.Canonical() != plan.Scope.Canonical() {
			return nil, planning.ErrConflict
		}
		view := PlanningReplanView{ID: attempt.Request.ID, RunID: attempt.Request.RunID, BaseRevision: p.Revision, BaseDigest: p.Digest, Status: "pending"}
		outcome, readErr := o.config.Store.GetPlanReplanOutcome(ctx, view.ID)
		if readErr != nil {
			return nil, readErr
		}
		if outcome != nil {
			if outcome.Validate() != nil || outcome.AttemptID != view.ID || outcome.AdmissionDigest != attempt.RequestDigest {
				return nil, planning.ErrConflict
			}
			view.Status = "rejected"
			if outcome.Abandonment != nil {
				view.Status = "abandoned"
			}
			if outcome.Proposed != nil {
				view.Status = "proposed"
				view.ProposedDigest = outcome.Proposed.Digest
				for _, task := range outcome.Proposed.Tasks {
					view.ProposedTasks = append(view.ProposedTasks, PlanningTaskView{ID: task.ID, Owner: task.Owner, Instruction: task.Instruction, DependsOn: task.DependsOn, Budget: task.Budget, ReviewRequired: task.Review != nil, Status: planning.StatusPending, NextAction: "proposal"})
				}
			}
		}
		committed, readErr := o.config.Store.GetPlanReplanCommit(ctx, view.ID)
		if readErr != nil {
			return nil, readErr
		}
		if committed != nil {
			if planning.Validate(*committed, planning.Limits{}) != nil || committed.ID != plan.ID || committed.PreviousDigest != p.Digest || committed.Scope.Canonical() != plan.Scope.Canonical() {
				return nil, planning.ErrConflict
			}
			view.Status = "committed"
			view.AcceptedRevision = committed.Revision
		}
		views = append(views, view)
	}
	return views, nil
}
