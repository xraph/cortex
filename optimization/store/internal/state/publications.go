package state

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
)

// GetLesson reads local eligibility independently of remote publication progress.
func (s *Store) GetLesson(ctx context.Context, lessonID id.ID) (optimization.Lesson, error) {
	var empty optimization.Lesson
	key, err := contextScope(ctx)
	if err != nil {
		return empty, err
	}
	unlock := s.lockOperation()
	defer unlock()
	if s.closed {
		return empty, optimization.ErrUnavailable
	}
	data, loadErr := s.loadResource(ctx, key, RecordKey{Kind: "lessons", Key: lessonID.String()})
	if loadErr != nil {
		return empty, loadErr
	}
	if data != nil {
		if value, exists := data.Lessons[lessonID.String()]; exists {
			return clone(value)
		}
	}
	return empty, optimization.ErrNotFound
}

// ResolveLesson rechecks a frozen reference and its transitive source eligibility.
func (s *Store) ResolveLesson(ctx context.Context, ref optimization.LessonRef) (optimization.Lesson, error) {
	var empty optimization.Lesson
	key, err := contextScope(ctx)
	if err != nil {
		return empty, err
	}
	unlock := s.lockOperation()
	defer unlock()
	if s.closed {
		return empty, optimization.ErrUnavailable
	}
	data, loadErr := s.loadScope(ctx, key)
	if loadErr != nil {
		return empty, loadErr
	}
	if data == nil {
		return empty, optimization.ErrNotFound
	}
	experiment := optimization.Experiment{LessonIDs: []id.ID{ref.ID}, FrozenLessons: []optimization.LessonRef{ref}}
	if lessonErr := checkLessonBindings(data, experiment, s.now().UTC()); lessonErr != nil {
		return empty, lessonErr
	}
	return clone(data.Lessons[ref.ID.String()])
}

// GetPublication reads one immutable payload and its delivery metadata.
func (s *Store) GetPublication(ctx context.Context, lessonID id.ID, revision int64) (optimization.Publication, error) {
	var empty optimization.Publication
	key, err := contextScope(ctx)
	if err != nil {
		return empty, err
	}
	if revision < 1 {
		return empty, optimization.ErrInvalidSpec
	}
	unlock := s.lockOperation()
	defer unlock()
	if s.closed {
		return empty, optimization.ErrUnavailable
	}
	data, loadErr := s.loadResource(ctx, key, RecordKey{Kind: "publications", Key: publicationKey(lessonID, revision)})
	if loadErr != nil {
		return empty, loadErr
	}
	if data != nil {
		if value, exists := data.Publications[publicationKey(lessonID, revision)]; exists {
			return clone(value)
		}
	}
	return empty, optimization.ErrNotFound
}

func publicationKey(lessonID id.ID, revision int64) string {
	return lessonID.String() + "/" + strconv.FormatInt(revision, 10)
}

func (tx *transaction) PutPublication(request optimization.PublicationRequest) error {
	if !tx.active {
		return optimization.ErrConflict
	}
	if err := tx.ctx.Err(); err != nil {
		return err
	}
	if err := optimization.ValidateLessonDraft(request.Lesson); err != nil {
		return err
	}
	key, err := optimization.ScopeKey(request.Lesson.Scope)
	if err != nil {
		return err
	}
	if key != tx.scope {
		return optimization.ErrPolicyDenied
	}
	if strings.TrimSpace(request.Actor.Subject) == "" || request.RemoteAggregateID == "" || strings.TrimSpace(request.RemoteAggregateID) != request.RemoteAggregateID || len(request.RemoteAggregateID) > 512 {
		return optimization.ErrInvalidSpec
	}
	lessonKey := request.Lesson.ID.String()
	if prior, exists := tx.data.PublicationRequests[lessonKey]; exists {
		if !same(prior, request) {
			return optimization.ErrConflict
		}
		if lesson := tx.data.Lessons[lessonKey]; !lesson.RevokedAt.IsZero() {
			return optimization.ErrStaleVersion
		}
		return checkLessonBindings(tx.data, tx.data.Experiments[request.ExperimentID.String()], tx.now)
	}
	experiment, exists := tx.data.Experiments[request.ExperimentID.String()]
	if !exists {
		return optimization.ErrNotFound
	}
	if experiment.Phase != optimization.PhaseReflect || !tx.validLease(experiment, request.Lease) {
		return optimization.ErrConflict
	}
	if lessonErr := checkLessonBindings(tx.data, experiment, tx.now); lessonErr != nil {
		return lessonErr
	}
	if evidenceErr := tx.validateLessonEvidence(experiment, request.Lesson); evidenceErr != nil {
		return evidenceErr
	}
	if !request.Lesson.ExpiresAt.IsZero() && !request.Lesson.ExpiresAt.After(tx.now) {
		return optimization.ErrInvalidSpec
	}
	for _, publication := range tx.data.Publications {
		if publication.RemoteAggregateID == request.RemoteAggregateID {
			return optimization.ErrConflict
		}
	}
	lesson := request.Lesson
	lesson.Revision, lesson.CreatedAt = 1, tx.now
	digest, err := optimization.LessonDigest(lesson)
	if err != nil {
		return err
	}
	publication := optimization.Publication{CreatedAt: tx.now, ExperimentID: experiment.ID, LessonID: lesson.ID, Operation: optimization.PublicationPublish, Payload: lesson, PayloadDigest: digest, LocalRevision: 1, RemoteAggregateID: request.RemoteAggregateID}
	detachedLesson, err := clone(lesson)
	if err != nil {
		return err
	}
	detachedPublication, err := clone(publication)
	if err != nil {
		return err
	}
	detachedRequest, err := clone(request)
	if err != nil {
		return err
	}
	if tx.data.Lessons == nil {
		tx.data.Lessons = make(map[string]optimization.Lesson)
	}
	if tx.data.Publications == nil {
		tx.data.Publications = make(map[string]optimization.Publication)
	}
	if tx.data.PublicationRequests == nil {
		tx.data.PublicationRequests = make(map[string]optimization.PublicationRequest)
	}
	tx.data.Lessons[lessonKey] = detachedLesson
	tx.data.Publications[publicationKey(lesson.ID, 1)] = detachedPublication
	tx.data.PublicationRequests[lessonKey] = detachedRequest
	event := tx.appendEvent(experiment, "lesson_publication_queued", request.Actor)
	event.Resources = append(event.Resources, lesson.ID)
	event.Resources = append(event.Resources, lesson.DecisionIDs...)
	return nil
}

func (tx *transaction) validateLessonEvidence(owner optimization.Experiment, lesson optimization.Lesson) error {
	if !slices.Contains(lesson.DecisionIDs, owner.DecisionID) {
		return optimization.ErrIncompleteEvidence
	}
	sources := make(map[string]bool)
	for _, decisionID := range lesson.DecisionIDs {
		decision, exists := tx.data.Decisions[decisionID.String()]
		if !exists {
			return optimization.ErrIncompleteEvidence
		}
		source, exists := tx.data.Experiments[decision.ExperimentID.String()]
		if !exists || source.DecisionID != decision.ID {
			return optimization.ErrIncompleteEvidence
		}
		if err := checkLessonBindings(tx.data, source, tx.now); err != nil {
			return err
		}
		switch lesson.EvidenceClass {
		case optimization.EvidenceValidated:
			if decision.EvidenceClass != optimization.EvidenceValidated || decision.Outcome != optimization.DecisionRecommend || !decision.Comparison.Complete {
				return optimization.ErrIncompleteEvidence
			}
		case optimization.EvidenceContrastive:
			if decision.EvidenceClass != optimization.EvidenceContrastive || !decision.Comparison.Complete {
				return optimization.ErrIncompleteEvidence
			}
		}
		sources[source.ID.String()] = true
	}
	for _, trialID := range lesson.TrialIDs {
		trial, exists := tx.data.Trials[trialID.String()]
		if !exists || !sources[trial.ExperimentID.String()] {
			return optimization.ErrIncompleteEvidence
		}
		switch trial.State {
		case optimization.TrialCompleted, optimization.TrialFailed, optimization.TrialCancelled, optimization.TrialBlocked:
		default:
			return optimization.ErrIncompleteEvidence
		}
	}
	return nil
}
