package memory

import (
	"slices"
	"strings"

	"github.com/xraph/cortex/optimization"
)

func (tx *transaction) RevokeLesson(request optimization.LessonRevocation) error {
	if !tx.active {
		return optimization.ErrConflict
	}
	if err := tx.ctx.Err(); err != nil {
		return err
	}
	if request.ExpectedRevision != 1 || strings.TrimSpace(request.Actor.Subject) == "" || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 4096 {
		return optimization.ErrInvalidSpec
	}
	key := request.LessonID.String()
	lesson, exists := tx.data.Lessons[key]
	if !exists {
		return optimization.ErrNotFound
	}
	if prior, found := tx.data.Revocations[key]; found {
		if prior != request {
			return optimization.ErrConflict
		}
		return nil
	}
	if lesson.Revision != request.ExpectedRevision || !lesson.RevokedAt.IsZero() {
		return optimization.ErrConflict
	}
	previous, exists := tx.data.Publications[publicationKey(lesson.ID, lesson.Revision)]
	if !exists {
		return optimization.ErrIncompleteEvidence
	}
	lesson.Revision++
	lesson.RevokedAt, lesson.RevocationReason = tx.now, request.Reason
	digest, err := optimization.LessonDigest(lesson)
	if err != nil {
		return err
	}
	publication := optimization.Publication{CreatedAt: tx.now, ExperimentID: previous.ExperimentID, LessonID: lesson.ID, Operation: optimization.PublicationRevoke, Payload: lesson, PayloadDigest: digest, LocalRevision: lesson.Revision, RemoteAggregateID: previous.RemoteAggregateID, RemoteVersion: previous.RemoteVersion}
	detachedLesson, err := clone(lesson)
	if err != nil {
		return err
	}
	detachedPublication, err := clone(publication)
	if err != nil {
		return err
	}
	previous.SupersededAt = tx.now
	tx.data.Lessons[key] = detachedLesson
	tx.data.Publications[publicationKey(lesson.ID, request.ExpectedRevision)] = previous
	tx.data.Publications[publicationKey(lesson.ID, lesson.Revision)] = detachedPublication
	if tx.data.Revocations == nil {
		tx.data.Revocations = make(map[string]optimization.LessonRevocation)
	}
	tx.data.Revocations[key] = request
	source := tx.data.Experiments[previous.ExperimentID.String()]
	event := tx.appendEvent(source, "lesson_revoked", request.Actor)
	event.Resources = append(event.Resources, lesson.ID)
	for experimentKey, experiment := range tx.data.Experiments {
		if !slices.Contains(experiment.LessonIDs, lesson.ID) {
			continue
		}
		experiment.Failure = "lesson_revoked"
		experiment.Revision++
		experiment.UpdatedAt = tx.now
		switch experiment.State {
		case optimization.StateCompleted, optimization.StateCancelled, optimization.StateFailed, optimization.StateBudgetExhausted:
		default:
			experiment.State = optimization.StatePaused
			experiment.Lease.ExpiresAt = tx.now
		}
		tx.data.Experiments[experimentKey] = experiment
		event = tx.appendEvent(experiment, "lesson_consumer_invalidated", request.Actor)
		event.Resources = append(event.Resources, lesson.ID)
	}
	return nil
}
