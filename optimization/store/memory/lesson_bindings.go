package memory

import (
	"time"

	"github.com/xraph/cortex/optimization"
)

func (tx *transaction) freezeLessons(experiment optimization.Experiment) ([]optimization.LessonRef, error) {
	refs := make([]optimization.LessonRef, 0, len(experiment.LessonIDs))
	seen := make(map[string]bool)
	for _, lessonID := range experiment.LessonIDs {
		key := lessonID.String()
		if seen[key] {
			return nil, optimization.ErrInvalidSpec
		}
		seen[key] = true
		lesson, exists := tx.data.Lessons[key]
		if !exists {
			return nil, optimization.ErrNotFound
		}
		if lesson.CreatedAt.After(experiment.Spec.LessonCutoff) || lesson.HarnessSchema != 1 {
			return nil, optimization.ErrStaleVersion
		}
		publication, exists := tx.data.Publications[publicationKey(lesson.ID, lesson.Revision)]
		if !exists {
			return nil, optimization.ErrIncompleteEvidence
		}
		refs = append(refs, optimization.LessonRef{ID: lessonID, Revision: lesson.Revision, Digest: publication.PayloadDigest})
	}
	experiment.FrozenLessons = refs
	if err := checkLessonBindings(tx.data, experiment, tx.now); err != nil {
		return nil, err
	}
	return refs, nil
}

func checkLessonBindings(data *scopeData, experiment optimization.Experiment, now time.Time) error {
	return checkBoundLessons(data, experiment, now, make(map[string]bool), make(map[string]bool))
}

func checkBoundLessons(data *scopeData, experiment optimization.Experiment, now time.Time, visiting, verified map[string]bool) error {
	if len(experiment.LessonIDs) != len(experiment.FrozenLessons) {
		return optimization.ErrIncompleteEvidence
	}
	for index, ref := range experiment.FrozenLessons {
		key := ref.ID.String()
		if ref.ID != experiment.LessonIDs[index] {
			return optimization.ErrIncompleteEvidence
		}
		lesson, exists := data.Lessons[key]
		if !exists || lesson.Revision != ref.Revision || !lesson.RevokedAt.IsZero() || !lesson.ExpiresAt.IsZero() && !now.Before(lesson.ExpiresAt) {
			return optimization.ErrStaleVersion
		}
		publication, exists := data.Publications[publicationKey(ref.ID, ref.Revision)]
		if !exists || publication.PayloadDigest != ref.Digest || publication.Operation != optimization.PublicationPublish || !publication.SupersededAt.IsZero() {
			return optimization.ErrStaleVersion
		}
		if verified[key] {
			continue
		}
		if visiting[key] {
			return optimization.ErrIncompleteEvidence
		}
		visiting[key] = true
		for _, decisionID := range lesson.DecisionIDs {
			decision, found := data.Decisions[decisionID.String()]
			if !found {
				return optimization.ErrIncompleteEvidence
			}
			source, found := data.Experiments[decision.ExperimentID.String()]
			if !found || source.Failure == "lesson_revoked" {
				return optimization.ErrStaleVersion
			}
			if err := checkBoundLessons(data, source, now, visiting, verified); err != nil {
				return err
			}
		}
		delete(visiting, key)
		verified[key] = true
	}
	return nil
}
