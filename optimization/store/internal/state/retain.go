package state

import "github.com/xraph/cortex/optimization"

func (tx *transaction) prepareRetainedBaseline(experiment optimization.Experiment, change optimization.Transition) (*optimization.RetainedBaseline, error) {
	if change.Retain == nil {
		return nil, nil
	}
	if change.Retain.DecisionID != experiment.DecisionID {
		return nil, optimization.ErrConflict
	}
	decision, exists := tx.data.Decisions[experiment.DecisionID.String()]
	if !exists || decision.ExperimentID != experiment.ID || decision.SpecDigest != experiment.SpecDigest {
		return nil, optimization.ErrIncompleteEvidence
	}
	baseline := experiment.Spec.Baseline
	if experiment.Spec.Improvement != nil {
		baseline = experiment.Spec.Improvement.Controllers[0]
	}
	if decision.Baseline != baseline {
		return nil, optimization.ErrIncompleteEvidence
	}
	for _, headID := range tx.data.ActivationHeads {
		head, found := tx.data.Activations[headID.String()]
		if !found {
			return nil, optimization.ErrIncompleteEvidence
		}
		if !head.Deactivated && head.DecisionID == decision.ID {
			return nil, optimization.ErrConflict
		}
	}
	return &optimization.RetainedBaseline{DecisionID: decision.ID, Baseline: baseline, Actor: change.Actor, Reason: change.Retain.Reason, CreatedAt: tx.now}, nil
}
