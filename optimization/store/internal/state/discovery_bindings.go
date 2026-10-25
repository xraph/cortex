package state

import (
	"context"
	"slices"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
)

// ResolveDiscoveryLessons validates a frozen earlier-task reference for its consumer.
func (s *Store) ResolveDiscoveryLessons(ctx context.Context, consumerID id.ID, ref optimization.DiscoveryRef) (optimization.DiscoveryLessons, error) {
	var empty optimization.DiscoveryLessons
	scope, err := contextScope(ctx)
	if err != nil {
		return empty, err
	}
	unlock := s.lockOperation()
	defer unlock()
	if s.closed {
		return empty, optimization.ErrUnavailable
	}
	data, err := s.loadScope(ctx, scope)
	if err != nil {
		return empty, err
	}
	if data == nil {
		return empty, optimization.ErrNotFound
	}
	consumer, exists := data.Experiments[consumerID.String()]
	if !exists {
		return empty, optimization.ErrNotFound
	}
	if !slices.Contains(consumer.Discoveries, ref) {
		return empty, optimization.ErrPolicyDenied
	}
	if bindingErr := checkLessonBindings(data, consumer, s.now().UTC()); bindingErr != nil {
		return empty, bindingErr
	}
	return clone(data.DiscoveryLessons[ref.ExperimentID.String()])
}

func (tx *transaction) freezeDiscoveries(experiment optimization.Experiment) ([]optimization.DiscoveryRef, error) {
	if len(experiment.Discoveries) != 0 {
		return nil, optimization.ErrInvalidSpec
	}
	link := experiment.Spec.Parent
	if link == nil || link.Sequence == 0 {
		return nil, nil
	}
	parent := tx.data.Experiments[link.ExperimentID.String()]
	if parent.Spec.Improvement == nil {
		return nil, optimization.ErrIncompleteEvidence
	}
	refs := make([]optimization.DiscoveryRef, 0, link.Sequence)
	for sequence := range link.Sequence {
		index := sequence*2 + link.Arm
		if index < 0 || index >= len(parent.Spec.Improvement.Children) {
			return nil, optimization.ErrPolicyDenied
		}
		sourceID := parent.Spec.Improvement.Children[index].ExperimentID
		source, exists := tx.data.DiscoveryLessons[sourceID.String()]
		if !exists {
			return nil, optimization.ErrIncompleteEvidence
		}
		refs = append(refs, optimization.DiscoveryRef{ExperimentID: sourceID, Digest: source.Digest})
	}
	return refs, nil
}

func checkDiscoveryBindings(data *scopeData, experiment optimization.Experiment, now time.Time, visiting, verified map[string]bool) error {
	link := experiment.Spec.Parent
	if link == nil {
		if len(experiment.Discoveries) != 0 {
			return optimization.ErrPolicyDenied
		}
		return nil
	}
	if link.Arm < 0 || link.Arm > 1 || link.Sequence < 0 || len(experiment.Discoveries) != link.Sequence {
		return optimization.ErrIncompleteEvidence
	}
	parent := data.Experiments[link.ExperimentID.String()]
	if parent.Spec.Improvement == nil {
		return optimization.ErrIncompleteEvidence
	}
	for sequence, ref := range experiment.Discoveries {
		index := sequence*2 + link.Arm
		if index >= len(parent.Spec.Improvement.Children) || parent.Spec.Improvement.Children[index].ExperimentID != ref.ExperimentID {
			return optimization.ErrPolicyDenied
		}
		source, exists := data.Experiments[ref.ExperimentID.String()]
		if !exists || source.Spec.Parent == nil || source.Spec.Parent.ExperimentID != parent.ID || source.Spec.Parent.Arm != link.Arm || source.Spec.Parent.Sequence != sequence || source.Spec.Controller != experiment.Spec.Controller {
			return optimization.ErrPolicyDenied
		}
		batch, exists := data.DiscoveryLessons[source.ID.String()]
		if !exists || batch.Digest != ref.Digest || batch.ExperimentID != source.ID || batch.ParentExperimentID != parent.ID || batch.Controller != source.Spec.Controller || batch.Arm != link.Arm || batch.Sequence != sequence {
			return optimization.ErrIncompleteEvidence
		}
		digest, err := optimization.DiscoveryLessonsDigest(batch)
		if err != nil || digest != ref.Digest {
			return optimization.ErrIncompleteEvidence
		}
		key := "discovery/" + source.ID.String()
		if verified[key] {
			continue
		}
		if visiting[key] {
			return optimization.ErrIncompleteEvidence
		}
		visiting[key] = true
		if source.Failure == "lesson_revoked" {
			return optimization.ErrStaleVersion
		}
		if sourceErr := checkBoundLessons(data, source, now, visiting, verified); sourceErr != nil {
			return sourceErr
		}
		if evidenceErr := checkDiscoveryEvidence(data, source, batch.Lessons, now); evidenceErr != nil {
			return evidenceErr
		}
		delete(visiting, key)
		verified[key] = true
	}
	return nil
}

func checkDiscoveryEvidence(data *scopeData, source optimization.Experiment, lessons []optimization.LessonDraft, now time.Time) error {
	for _, draft := range lessons {
		for _, trialID := range draft.EvidenceIDs {
			trial, exists := data.Trials[trialID.String()]
			if !exists || trial.ExperimentID != source.ID || trial.Split != optimization.SplitDiscovery {
				return optimization.ErrPolicyDenied
			}
			if !optimization.TrialEvidenceReady(trial) || trial.Execution == nil || trial.EvaluationID == "" {
				return optimization.ErrIncompleteEvidence
			}
			evaluation, exists := data.Evaluations[trial.EvaluationID]
			if !exists {
				return optimization.ErrIncompleteEvidence
			}
			if err := evaluation.ValidateCaptured(source, trial); err != nil {
				return err
			}
			if err := checkTrialArtifacts(data, trial, now); err != nil {
				return err
			}
		}
	}
	return nil
}
