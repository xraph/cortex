package state

import (
	"context"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
)

// GetDecision returns immutable selection evidence in the exact context scope.
func (s *Store) GetDecision(ctx context.Context, decisionID id.ID) (optimization.Decision, error) {
	var empty optimization.Decision
	key, err := contextScope(ctx)
	if err != nil {
		return empty, err
	}
	unlock := s.lockOperation()
	defer unlock()
	if s.closed {
		return empty, optimization.ErrUnavailable
	}
	data, loadErr := s.loadResource(ctx, key, RecordKey{Kind: "decisions", Key: decisionID.String()})
	if loadErr != nil {
		return empty, loadErr
	}
	if data != nil {
		if value, exists := data.Decisions[decisionID.String()]; exists {
			return clone(value)
		}
	}
	return empty, optimization.ErrNotFound
}

func (tx *transaction) prepareDecision(experiment optimization.Experiment, change optimization.Transition) (*optimization.Decision, error) {
	if change.Decision == nil {
		if experiment.Phase == optimization.PhaseDecide && change.NextPhase == optimization.PhaseReflect {
			return nil, optimization.ErrIncompleteEvidence
		}
		return nil, nil
	}
	if experiment.State != optimization.StateRunning || change.NextState != optimization.StateRunning || experiment.Phase != optimization.PhaseDecide || change.NextPhase != optimization.PhaseReflect || !experiment.DecisionID.IsNil() || len(change.Candidates)+len(change.Versions)+len(change.Trials)+len(change.TrialUpdates)+len(change.Evaluations) > 0 {
		return nil, optimization.ErrConflict
	}
	value := *change.Decision
	if _, err := optimization.ParseID(value.ID.String(), optimization.PrefixDecision); err != nil {
		return nil, err
	}
	if _, exists := tx.data.Decisions[value.ID.String()]; exists {
		return nil, optimization.ErrConflict
	}
	if value.ExperimentID != experiment.ID || value.SpecDigest != experiment.SpecDigest || value.Baseline != experiment.Spec.Baseline || value.SelectionKind != string(optimization.HarnessExecution) {
		return nil, optimization.ErrIncompleteEvidence
	}
	baseline, exists := tx.data.Versions[value.Baseline.ID.String()]
	if !exists || value.PolicyDigest != baseline.PolicyDigest {
		return nil, optimization.ErrPolicyDenied
	}
	allocation, exists := tx.data.Allocations[value.AllocationID.String()]
	if !exists || allocation.ExposedAt.IsZero() || !allocation.ReleasedAt.IsZero() {
		return nil, optimization.ErrIncompleteEvidence
	}
	matched := false
	for _, arm := range allocation.PairedRoster {
		if arm.ExperimentID == experiment.ID && arm.Baseline == value.Baseline && arm.Finalist == value.Candidate && arm.Controller == experiment.Spec.Controller {
			matched = true
		}
	}
	if !matched {
		return nil, optimization.ErrIncompleteEvidence
	}
	// Recheck the complete paired roster before any arm can publish a decision.
	validationExperiment := experiment
	validationExperiment.Phase = optimization.PhaseValidate
	validationChange := change
	validationChange.NextPhase = optimization.PhaseDecide
	if err := tx.validatePhaseEvidence(validationExperiment, validationChange, candidateResources{}, nil); err != nil {
		return nil, err
	}
	evidence := make([]optimization.DecisionEvidence, 0)
	for _, trial := range tx.data.Trials {
		if trial.ExperimentID != experiment.ID || trial.Split != optimization.SplitValidation || trial.AllocationID != allocation.ID {
			continue
		}
		if trial.Version != value.Baseline && trial.Version != value.Candidate {
			return nil, optimization.ErrIncompleteEvidence
		}
		item := optimization.DecisionEvidence{Trial: trial}
		if trial.EvaluationID != "" {
			evaluation, found := tx.data.Evaluations[trial.EvaluationID]
			if !found {
				return nil, optimization.ErrIncompleteEvidence
			}
			item.Evaluation = &evaluation
		}
		evidence = append(evidence, item)
	}
	if err := optimization.ValidateDecisionEvidence(experiment, value, evidence); err != nil {
		return nil, err
	}
	if value.Outcome != optimization.DecisionInconclusive {
		if err := checkDecisionArtifacts(tx.data, value, tx.now); err != nil {
			return nil, err
		}
	}
	value.CreatedAt = tx.now
	detached, err := clone(value)
	if err != nil {
		return nil, err
	}
	return &detached, nil
}
