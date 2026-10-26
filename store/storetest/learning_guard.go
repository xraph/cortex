package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/learning"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

type lessonAuthority struct{ denied bool }

func (a *lessonAuthority) AuthorizeLesson(context.Context, learning.Action, learning.Resource) error {
	if a.denied {
		return learning.ErrDenied
	}
	return nil
}
func (a *lessonAuthority) AuthorizeResultContent(context.Context, verification.ResultContentRequest) error {
	if a.denied {
		return learning.ErrDenied
	}
	return nil
}
func (*lessonAuthority) WithCommitFence(ctx context.Context, binding verification.Binding, next func(verification.Binding) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return next(binding)
}
func (*lessonAuthority) Now() time.Time { return time.Now().UTC() }

// QualifiedLearningGuard checks withdrawal and grants around content hydration.
func QualifiedLearningGuard(t *testing.T, s store.Store) {
	repository := resultCapability[learning.Repository](t, s)
	authority := &lessonAuthority{}
	guard, err := learning.NewGuard(learning.GuardConfig{Repository: repository, Results: resultCapability[verification.ResultContentStore](t, s), ContentAuthority: authority, Authority: authority, Fence: authority, Clock: authority})
	require.NoError(t, err)
	ctx, request := AcceptedLessonFixture(t, s)
	authority.denied = true
	_, err = guard.Accept(ctx, request)
	require.ErrorIs(t, err, learning.ErrDenied)
	_, err = repository.GetLesson(ctx, request.ID)
	require.ErrorIs(t, err, learning.ErrNotFound)
	authority.denied = false
	publication, err := guard.Accept(ctx, request)
	require.NoError(t, err)
	ref := learning.Ref{ID: request.ID, Revision: 1, Digest: publication.PayloadDigest}
	_, err = guard.WithLesson(ctx, ref, learning.ActionRead, func(context.Context, learning.Lesson) error { authority.denied = true; return nil })
	require.ErrorIs(t, err, learning.ErrDenied)
	authority.denied = false
	_, err = guard.WithLesson(ctx, ref, learning.ActionRead, func(operation context.Context, _ learning.Lesson) error {
		_, revokeErr := repository.RevokeLesson(operation, learning.RevokeRequest{ID: request.ID, ExpectedRevision: 1, Actor: "reviewer", Reason: "withdrawn"})
		return revokeErr
	})
	require.ErrorIs(t, err, learning.ErrIneligible)
	authority.denied = true
	_, err = guard.Delete(ctx, learning.DeletionRequest{ID: request.ID, ExpectedRevision: 2, Actor: "retention-worker", Reason: "policy"})
	require.ErrorIs(t, err, learning.ErrDenied)
	authority.denied = false
	_, err = guard.Delete(ctx, learning.DeletionRequest{ID: request.ID, ExpectedRevision: 2, Actor: "retention-worker", Reason: "policy"})
	require.NoError(t, err)
}
