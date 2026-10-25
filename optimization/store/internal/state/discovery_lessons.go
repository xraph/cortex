package state

import (
	"context"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
)

// GetDiscoveryLessons returns one immutable comparison-local reflection.
func (s *Store) GetDiscoveryLessons(ctx context.Context, experimentID id.ID) (optimization.DiscoveryLessons, error) {
	var empty optimization.DiscoveryLessons
	key, err := contextScope(ctx)
	if err != nil {
		return empty, err
	}
	unlock := s.lockOperation()
	defer unlock()
	if s.closed {
		return empty, optimization.ErrUnavailable
	}
	data, err := s.loadResource(ctx, key, RecordKey{Kind: "discovery_lessons", Key: experimentID.String()})
	if err != nil {
		return empty, err
	}
	if data != nil {
		if value, exists := data.DiscoveryLessons[experimentID.String()]; exists {
			return clone(value)
		}
	}
	return empty, optimization.ErrNotFound
}

func (tx *transaction) PutDiscoveryLessons(request optimization.DiscoveryLessonsRequest) (optimization.DiscoveryLessons, error) {
	var empty optimization.DiscoveryLessons
	if !tx.active {
		return empty, optimization.ErrConflict
	}
	if err := tx.ctx.Err(); err != nil {
		return empty, err
	}
	if err := request.Validate(); err != nil {
		return empty, err
	}
	child, exists := tx.data.Experiments[request.ExperimentID.String()]
	if !exists {
		return empty, optimization.ErrNotFound
	}
	if !tx.validLease(child, request.Lease) {
		return empty, optimization.ErrConflict
	}
	link := child.Spec.Parent
	if link == nil {
		return empty, optimization.ErrPolicyDenied
	}
	parent := tx.data.Experiments[link.ExperimentID.String()]
	if parent.Spec.Improvement == nil || link.Arm < 0 || link.Arm > 1 || link.Sequence < 0 || link.Sequence >= len(parent.Spec.Improvement.Children)/2 {
		return empty, optimization.ErrPolicyDenied
	}
	index := link.Sequence*2 + link.Arm
	if parent.Spec.Improvement.Children[index].ExperimentID != child.ID || child.Spec.Controller != parent.Spec.Improvement.Controllers[link.Arm] {
		return empty, optimization.ErrPolicyDenied
	}
	if previous, ok := tx.data.DiscoveryLessons[child.ID.String()]; ok {
		if previous.Domain != request.Domain || !same(previous.Lessons, request.Lessons) {
			return empty, optimization.ErrConflict
		}
		return clone(previous)
	}
	if parent.Phase != optimization.PhaseBaseline || child.Phase != optimization.PhaseValidate {
		return empty, optimization.ErrPolicyDenied
	}
	if err := checkLessonBindings(tx.data, child, tx.now); err != nil {
		return empty, err
	}
	for _, draft := range request.Lessons {
		for _, trialID := range draft.EvidenceIDs {
			trial, ok := tx.data.Trials[trialID.String()]
			if !ok || trial.ExperimentID != child.ID || trial.Split != optimization.SplitDiscovery {
				return empty, optimization.ErrPolicyDenied
			}
			if !optimization.TrialEvidenceReady(trial) || trial.Execution == nil || trial.EvaluationID == "" {
				return empty, optimization.ErrIncompleteEvidence
			}
			evaluation, ok := tx.data.Evaluations[trial.EvaluationID]
			if !ok {
				return empty, optimization.ErrIncompleteEvidence
			}
			if err := evaluation.ValidateCaptured(child, trial); err != nil {
				return empty, err
			}
			if err := checkTrialArtifacts(tx.data, trial, tx.now); err != nil {
				return empty, err
			}
		}
	}
	value := optimization.DiscoveryLessons{ExperimentID: child.ID, ParentExperimentID: parent.ID, Controller: child.Spec.Controller, Arm: link.Arm, Sequence: link.Sequence, Domain: request.Domain, Lessons: request.Lessons, CreatedAt: tx.now}
	var err error
	value.Digest, err = optimization.DiscoveryLessonsDigest(value)
	if err != nil {
		return empty, err
	}
	value, err = clone(value)
	if err != nil {
		return empty, err
	}
	if tx.data.DiscoveryLessons == nil {
		tx.data.DiscoveryLessons = map[string]optimization.DiscoveryLessons{}
	}
	tx.data.DiscoveryLessons[child.ID.String()] = value
	event := tx.appendEvent(child, "discovery_lessons_recorded", child.Spec.Requester)
	event.Resources = append(event.Resources, parent.ID, child.Spec.Controller.ID)
	return clone(value)
}
