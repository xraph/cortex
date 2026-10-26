package operator

import (
	"context"

	opt "github.com/xraph/cortex/optimization"
)

// RetryLessonInput identifies the local outbox payload and last delivery attempt.
// The transport supplies the command key separately from these payload fields.
type RetryLessonInput struct {
	ID                 string `json:"id" path:"id"`
	LocalRevision      int64  `json:"local_revision"`
	ExpectedGeneration int64  `json:"expected_generation"`
	Reason             string `json:"reason"`
}

// RevokeLessonInput withdraws a lesson at its current local revision.
type RevokeLessonInput struct {
	ID               string `json:"id" path:"id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason"`
}

// RetryLesson queues another bounded delivery cycle without calling Fabriq.
func (c Commands) RetryLesson(ctx context.Context, in RetryLessonInput, key string) (opt.Publication, error) {
	if c.Optimizer == nil {
		return opt.Publication{}, opt.ErrUnavailable
	}
	lessonID, err := opt.ParseID(in.ID, opt.PrefixLesson)
	if err != nil {
		return opt.Publication{}, err
	}
	return c.Optimizer.RetryLessonCommand(ctx, opt.RetryLessonRequest{LessonID: lessonID, LocalRevision: in.LocalRevision, ExpectedGeneration: in.ExpectedGeneration, Reason: in.Reason, IdempotencyKey: key})
}

// RevokeLesson withdraws local eligibility and queues remote revocation atomically.
func (c Commands) RevokeLesson(ctx context.Context, in RevokeLessonInput, key string) (opt.Lesson, error) {
	if c.Optimizer == nil {
		return opt.Lesson{}, opt.ErrUnavailable
	}
	lessonID, err := opt.ParseID(in.ID, opt.PrefixLesson)
	if err != nil {
		return opt.Lesson{}, err
	}
	return c.Optimizer.RevokeLessonCommand(ctx, opt.RevokeLessonRequest{LessonID: lessonID, ExpectedRevision: in.ExpectedRevision, Reason: in.Reason, IdempotencyKey: key})
}
