package memory

import (
	"context"

	"github.com/xraph/cortex/optimization"
)

// GetEvaluation reads immutable captured scoring evidence in exact scope.
func (s *Store) GetEvaluation(ctx context.Context, evaluationID string) (optimization.Evaluation, error) {
	var empty optimization.Evaluation
	key, err := contextScope(ctx)
	if err != nil {
		return empty, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return empty, optimization.ErrUnavailable
	}
	if data := s.scopes[key]; data != nil {
		if value, exists := data.Evaluations[evaluationID]; exists {
			return clone(value)
		}
	}
	return empty, optimization.ErrNotFound
}

func (tx *transaction) prepareEvaluations(experiment optimization.Experiment, change optimization.Transition, trials map[string]optimization.Trial) (map[string]optimization.Evaluation, error) {
	result := make(map[string]optimization.Evaluation, len(change.Evaluations))
	if len(change.Evaluations) == 0 {
		return result, nil
	}
	if experiment.State != optimization.StateRunning || change.NextState != optimization.StateRunning {
		return nil, optimization.ErrConflict
	}
	for _, evaluation := range change.Evaluations {
		if _, exists := tx.data.Evaluations[evaluation.ID]; exists {
			return nil, optimization.ErrConflict
		}
		if _, exists := result[evaluation.ID]; exists {
			return nil, optimization.ErrConflict
		}
		key := evaluation.TrialID.String()
		// Scoring always consumes a separately committed execution record.
		if _, changed := trials[key]; changed {
			return nil, optimization.ErrConflict
		}
		trial, exists := tx.data.Trials[key]
		if !exists || trial.ExperimentID != experiment.ID {
			return nil, optimization.ErrNotFound
		}
		if trial.EvaluationID != "" {
			return nil, optimization.ErrConflict
		}
		if tx.original == nil {
			return nil, optimization.ErrIncompleteEvidence
		}
		original, exists := tx.original.Trials[key]
		if !exists || original.Execution == nil || trial.Execution == nil || original.Execution.Digest != trial.Execution.Digest {
			return nil, optimization.ErrIncompleteEvidence
		}
		if trial.State != optimization.TrialCompleted && trial.State != optimization.TrialFailed && trial.State != optimization.TrialCancelled {
			return nil, optimization.ErrIncompleteEvidence
		}
		if err := tx.validateTrialPosition(experiment, trial, experiment.Phase, candidateResources{}); err != nil {
			return nil, err
		}
		if err := evaluation.ValidateCaptured(experiment, trial); err != nil {
			return nil, err
		}
		evaluation.RecordedAt = tx.now
		detached, err := clone(evaluation)
		if err != nil {
			return nil, err
		}
		result[evaluation.ID] = detached
		trial.EvaluationID = evaluation.ID
		trial.Revision++
		trials[key] = trial
	}
	return result, nil
}
