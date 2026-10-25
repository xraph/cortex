package optimization

import (
	"context"
	"strings"
	"time"
)

// CreateWithLessons recalls one complete bounded result set before creating a
// draft. The cutoff must already be frozen in the supplied specification. Store
// creation atomically verifies the recalled revisions and digests again.
func (o *Optimizer) CreateWithLessons(ctx context.Context, bundle ExperimentBundle, query RecallQuery) (Experiment, error) {
	var empty Experiment
	if o.config.Lessons == nil {
		return empty, ErrUnavailable
	}
	frozen, err := o.creationBundle(ctx, bundle)
	if err != nil {
		return empty, err
	}
	if len(frozen.Experiment.LessonIDs) != 0 || len(frozen.Experiment.FrozenLessons) != 0 || frozen.ExpectedLessons != nil {
		return empty, ErrInvalidSpec
	}
	request, err := copyModelJSON(&query, 64<<10)
	if err != nil {
		return empty, err
	}
	scope, err := ScopeKey(request.Scope)
	if err != nil || scope != frozen.Experiment.Spec.Scope.Canonical() {
		return empty, ErrPolicyDenied
	}
	if !request.Cutoff.Equal(frozen.Experiment.Spec.LessonCutoff) || request.Cutoff.IsZero() || request.Cutoff.After(time.Now()) || request.HarnessSchema != 1 || strings.TrimSpace(request.Domain) == "" || len(request.Domain) > 256 || strings.TrimSpace(request.Query) == "" || len(request.Query) > 32768 || request.Limit < 1 || request.Limit > 100 || request.Cursor != "" {
		return empty, ErrInvalidSpec
	}
	if _, err = Preflight(frozen.Experiment.Spec); err != nil {
		return empty, err
	}
	if planErr := frozen.Experiment.EvaluationPlan.Validate(frozen.Experiment.Spec); planErr != nil {
		return empty, planErr
	}
	page, err := o.config.Lessons.Recall(copyScopeContext(ctx), *request)
	if err != nil {
		return empty, err
	}
	detached, err := copyModelJSON(&page, 16<<20)
	if err != nil {
		return empty, err
	}
	if detached.NextCursor != "" || len(detached.Items) > request.Limit {
		return empty, ErrIncompleteEvidence
	}
	frozen.ExpectedLessons = make([]LessonRef, 0, len(detached.Items))
	seen := make(map[string]bool, len(detached.Items))
	for _, lesson := range detached.Items {
		if seen[lesson.ID.String()] {
			return empty, ErrIncompleteEvidence
		}
		seen[lesson.ID.String()] = true
		if lesson.Scope.Canonical() != scope || lesson.Domain != request.Domain || lesson.HarnessSchema != request.HarnessSchema || lesson.CreatedAt.After(request.Cutoff) {
			return empty, ErrIncompleteEvidence
		}
		ref, resolveErr := o.resolveRecalledLesson(ctx, lesson)
		if resolveErr != nil {
			return empty, resolveErr
		}
		frozen.Experiment.LessonIDs = append(frozen.Experiment.LessonIDs, ref.ID)
		frozen.ExpectedLessons = append(frozen.ExpectedLessons, ref)
	}
	// Create repeats caller authorization after external recall and provenance reads.
	return o.Create(ctx, *frozen)
}

func (o *Optimizer) resolveRecalledLesson(ctx context.Context, lesson Lesson) (LessonRef, error) {
	ref := LessonRef{ID: lesson.ID, Revision: lesson.Revision}
	digest, err := LessonDigest(lesson)
	if err != nil {
		return ref, err
	}
	ref.Digest = digest
	publication, err := o.config.Repository.GetPublication(copyScopeContext(ctx), lesson.ID, lesson.Revision)
	if err != nil {
		return ref, err
	}
	if publication.PayloadDigest != digest || publication.Operation != PublicationPublish {
		return ref, ErrIncompleteEvidence
	}
	resolved, cleanup, err := o.meter.authorizeAction(ctx, publication.ExperimentID, ActionRead)
	if err != nil {
		return ref, err
	}
	defer cleanup()
	_, err = o.config.Repository.ResolveLesson(resolved, ref)
	return ref, err
}
