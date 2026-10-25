package optimization

import (
	"context"

	"github.com/xraph/cortex/id"
)

func (h *ImprovementPhaseHandler) decide(ctx context.Context, item WorkItem) (Transition, error) {
	evaluator, ok := h.child.config.ImprovementEvaluator.(ControllerEvaluator)
	if !ok {
		return Transition{}, ErrUnavailable
	}
	parent := item.Experiment
	repo := h.child.config.Repository
	page, err := repo.ListValidationAllocations(ctx, parent.ID, PageQuery{Limit: 100})
	if err != nil {
		return Transition{}, err
	}
	if len(page.Items) != 1 || page.NextCursor != "" {
		return Transition{}, ErrIncompleteEvidence
	}
	allocation := page.Items[0]
	if authErr := h.authorizeComparisonChildren(ctx, parent); authErr != nil {
		return Transition{}, authErr
	}
	evidence, err := ReadControllerEvidence(ctx, repo, parent, allocation)
	if err != nil {
		return Transition{}, err
	}
	request, err := BuildControllerComparison(parent, allocation, evidence)
	if err != nil {
		return Transition{}, err
	}
	detached, err := copyModelJSON(&request, 16<<20)
	if err != nil {
		return Transition{}, err
	}
	comparison, err := evaluator.CompareControllers(ctx, *detached)
	if err != nil {
		return Transition{}, err
	}
	if authErr := h.authorizeComparisonChildren(ctx, parent); authErr != nil {
		return Transition{}, authErr
	}
	baseline, err := repo.GetVersion(ctx, parent.Spec.Improvement.Controllers[0].ID)
	if err != nil {
		return Transition{}, err
	}
	if baseline.Kind != HarnessImprovement || baseline.Digest != parent.Spec.Improvement.Controllers[0].Digest {
		return Transition{}, ErrIncompleteEvidence
	}
	decision := Decision{ID: id.New(PrefixDecision), ExperimentID: parent.ID, SpecDigest: parent.SpecDigest, AllocationID: allocation.ID, Baseline: parent.Spec.Improvement.Controllers[0], Candidate: parent.Spec.Improvement.Controllers[1], PolicyDigest: baseline.PolicyDigest, SelectionKind: string(HarnessImprovement), Outcome: DecisionInconclusive, EvidenceClass: EvidenceExploratory, Comparison: comparison}
	for _, group := range request.Groups {
		decision.EvaluationIDs = append(decision.EvaluationIDs, group.BaselineIDs...)
		decision.EvaluationIDs = append(decision.EvaluationIDs, group.CandidateIDs...)
	}
	selectDecisionOutcome(parent.Spec, &decision)
	if validationErr := ValidateControllerDecisionEvidence(parent, allocation, decision, evidence); validationErr != nil {
		return Transition{}, validationErr
	}
	return Transition{NextState: StateRunning, NextPhase: PhaseReflect, Decision: &decision}, nil
}

func (h *ImprovementPhaseHandler) authorizeComparisonChildren(ctx context.Context, parent Experiment) error {
	for _, template := range parent.Spec.Improvement.Children {
		child, err := h.childSnapshot(ctx, template)
		if err != nil {
			return err
		}
		if child.State != StateRunning || child.Phase != PhaseValidate && child.Phase != PhaseDecide {
			return ErrIncompleteEvidence
		}
	}
	return nil
}

func (h *ImprovementPhaseHandler) review(ctx context.Context, item WorkItem) (Transition, error) {
	parent := item.Experiment
	if parent.DecisionID.IsNil() {
		return Transition{}, ErrIncompleteEvidence
	}
	if err := h.authorizeComparisonChildren(ctx, parent); err != nil {
		return Transition{}, err
	}
	repo := h.child.config.Repository
	decision, err := repo.GetDecision(ctx, parent.DecisionID)
	if err != nil {
		return Transition{}, err
	}
	allocation, err := repo.GetValidationAllocation(ctx, decision.AllocationID)
	if err != nil {
		return Transition{}, err
	}
	evidence, err := ReadControllerEvidence(ctx, repo, parent, allocation)
	if err != nil {
		return Transition{}, err
	}
	if err := ValidateControllerDecisionEvidence(parent, allocation, decision, evidence); err != nil {
		return Transition{}, err
	}
	// Discovery learning is already frozen per arm. Finishing the parent does not
	// expose held-out answers to a controller or publish its private lessons.
	return Transition{NextState: StateReviewRequired}, nil
}
