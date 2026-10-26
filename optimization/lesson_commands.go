package optimization

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// RetryLessonRequest identifies a bounded delivery cycle and its operator command.
type RetryLessonRequest struct {
	LessonID           id.ID  `json:"lesson_id"`
	LocalRevision      int64  `json:"local_revision"`
	ExpectedGeneration int64  `json:"expected_generation"`
	Reason             string `json:"reason"`
	IdempotencyKey     string `json:"idempotency_key"`
}

// RevokeLessonRequest withdraws local eligibility before remote delivery resumes.
type RevokeLessonRequest struct {
	LessonID         id.ID  `json:"lesson_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason"`
	IdempotencyKey   string `json:"idempotency_key"`
}

// RetryLessonCommand queues another bounded cycle without calling the destination.
// Replays return current publication state, including any later delivery outcome.
func (o *Optimizer) RetryLessonCommand(ctx context.Context, request RetryLessonRequest) (Publication, error) {
	if err := allowOptimizationInvocation(ctx); err != nil {
		return Publication{}, err
	}
	key, err := lessonCommandKey(request.LessonID, request.IdempotencyKey, request.Reason)
	if err != nil {
		return Publication{}, err
	}
	if request.LocalRevision < 1 || request.ExpectedGeneration < 1 {
		return Publication{}, ErrInvalidSpec
	}
	return o.lessonOperator().retry(ctx, PublicationRetry{LessonID: request.LessonID, LocalRevision: request.LocalRevision, ExpectedGeneration: request.ExpectedGeneration, Reason: request.Reason, CommandKey: key})
}

// RevokeLessonCommand commits local withdrawal and its outbox atomically. It
// requires the current operator's grant without resolving the original requester.
func (o *Optimizer) RevokeLessonCommand(ctx context.Context, request RevokeLessonRequest) (Lesson, error) {
	if err := allowOptimizationInvocation(ctx); err != nil {
		return Lesson{}, err
	}
	key, err := lessonCommandKey(request.LessonID, request.IdempotencyKey, request.Reason)
	if err != nil {
		return Lesson{}, err
	}
	if request.ExpectedRevision < 1 {
		return Lesson{}, ErrInvalidSpec
	}
	actor, err := o.caller(ctx)
	if err != nil {
		return Lesson{}, err
	}
	lesson, err := o.config.Repository.GetLesson(copyScopeContext(ctx), request.LessonID)
	if err != nil {
		return Lesson{}, err
	}
	if lesson.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return Lesson{}, ErrPolicyDenied
	}
	resource := Resource{Scope: copyScope(lesson.Scope), ID: lesson.ID, Kind: "lesson"}
	if authErr := o.config.Authorizer.Authorize(copyScopeContext(ctx), ActionRevokeLesson, resource); authErr != nil {
		return Lesson{}, authErr
	}
	if revokeErr := o.config.Repository.Transact(ctx, func(tx Transaction) error {
		return tx.RevokeLesson(LessonRevocation{LessonID: lesson.ID, ExpectedRevision: request.ExpectedRevision, Actor: actor, Reason: request.Reason, CommandKey: key})
	}); revokeErr != nil {
		return Lesson{}, revokeErr
	}
	return o.config.Repository.GetLesson(ctx, lesson.ID)
}

func lessonCommandKey(lessonID id.ID, key, reason string) (string, error) {
	if _, err := ParseID(lessonID.String(), PrefixLesson); err != nil {
		return "", err
	}
	if !validReviewReason(reason) {
		return "", ErrInvalidSpec
	}
	return operatorCommandKey(key)
}

type lessonOperator struct {
	repository Repository
	authorizer Authorizer
	caller     CallerIdentity
	meter      *LedgerMeter
}

func (o *Optimizer) lessonOperator() lessonOperator {
	return lessonOperator{repository: o.config.Repository, authorizer: o.config.Authorizer, caller: o.config.Caller, meter: o.meter}
}

func (s lessonOperator) retry(ctx context.Context, request PublicationRetry) (Publication, error) {
	var empty Publication
	if err := allowOptimizationInvocation(ctx); err != nil {
		return empty, err
	}
	actor, err := currentCaller(ctx, s.caller)
	if err != nil {
		return empty, err
	}
	request.Actor = actor
	value, err := s.repository.GetPublication(copyScopeContext(ctx), request.LessonID, request.LocalRevision)
	if err != nil {
		return empty, err
	}
	if value.Payload.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return empty, ErrPolicyDenied
	}
	resource := Resource{Scope: copyScope(cortex.ScopeFromContext(ctx)), ID: request.LessonID, Kind: "lesson"}
	if authErr := s.authorizer.Authorize(copyScopeContext(ctx), ActionRetryPublication, resource); authErr != nil {
		return empty, authErr
	}
	action := ActionPublishLesson
	if value.Operation == PublicationRevoke {
		action = ActionRevokeLesson
	}
	resolved, cleanup, err := s.meter.authorizeAction(ctx, value.ExperimentID, action)
	if err != nil {
		return empty, err
	}
	defer cleanup()
	current, err := currentCaller(ctx, s.caller)
	if err != nil {
		return empty, err
	}
	if current != actor {
		return empty, ErrPolicyDenied
	}
	if authErr := s.authorizer.Authorize(copyScopeContext(ctx), ActionRetryPublication, resource); authErr != nil {
		return empty, authErr
	}
	if retryErr := s.repository.Transact(resolved, func(tx Transaction) error { return tx.RetryPublication(request) }); retryErr != nil {
		return empty, retryErr
	}
	return s.repository.GetPublication(resolved, request.LessonID, request.LocalRevision)
}
