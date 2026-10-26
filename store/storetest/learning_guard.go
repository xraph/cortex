package storetest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/learning"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

type lessonAuthority struct {
	denied bool
	hook   func(learning.Action, learning.Resource) error
}

func (a *lessonAuthority) AuthorizeLesson(_ context.Context, action learning.Action, resource learning.Resource) error {
	if a.denied {
		return learning.ErrDenied
	}
	if a.hook != nil {
		return a.hook(action, resource)
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
	draft := request
	draft.Result = verification.ResultReadRequest{}
	accepted, err := guard.AcceptResult(ctx, draft, verification.ResultContentRequest{SchemaVersion: 1, RunID: request.Result.RunID, ResultID: request.Result.ResultID})
	require.NoError(t, err)
	require.Equal(t, publication.LessonID, accepted.LessonID)
	page, err := guard.Publications(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	originalScope := cortex.ScopeFromContext(ctx).Canonical()
	authority.hook = func(_ learning.Action, resource learning.Resource) error {
		resource.Scope.Levels[0].Value = "mutated"
		return nil
	}
	require.NoError(t, guard.Authorize(ctx, learning.ActionRead, request.ID, request.Domain))
	require.Equal(t, originalScope, cortex.ScopeFromContext(ctx).Canonical())
	seen, revoked := false, false
	authority.hook = func(action learning.Action, resource learning.Resource) error {
		if action == learning.ActionRead {
			if resource.ID == request.ID {
				if revoked {
					return learning.ErrDenied
				}
				seen = true
			} else if resource.ID.IsNil() && seen {
				revoked = true
			}
		}
		return nil
	}
	deniedPage, readErr := guard.Publications(ctx, "", 10)
	require.ErrorIs(t, readErr, learning.ErrDenied)
	require.Empty(t, deniedPage.Items)
	authority.hook = nil
	body, err := json.Marshal(page)
	require.NoError(t, err)
	require.NotContains(t, string(body), publication.Payload.Statement)
	metadata, err := guard.Review(ctx, request.ID)
	require.NoError(t, err)
	require.Equal(t, request.Result.RunID, metadata.SourceRunID)
	claimed, err := repository.ClaimLessonPublication(ctx, request.ID, 1, "delivery")
	require.NoError(t, err)
	require.NoError(t, repository.CompleteLessonPublication(ctx, claimed, nil, "unavailable"))
	failed, err := repository.GetLessonPublication(ctx, request.ID, 1)
	require.NoError(t, err)
	authority.denied = true
	_, err = guard.Publications(ctx, "", 10)
	require.ErrorIs(t, err, learning.ErrDenied)
	require.ErrorIs(t, guard.Retry(ctx, request.ID, 1, claimed.Generation), learning.ErrDenied)
	authority.denied = false
	require.NoError(t, guard.Retry(ctx, request.ID, 1, failed.Generation))
	require.Error(t, guard.Retry(ctx, request.ID, 1, failed.Generation))
	ref := learning.Ref{ID: request.ID, Revision: 1, Digest: publication.PayloadDigest}
	_, err = guard.WithLesson(ctx, ref, learning.ActionRead, func(context.Context, learning.Lesson) error { authority.denied = true; return nil })
	require.ErrorIs(t, err, learning.ErrDenied)
	authority.denied = false
	_, err = guard.WithLesson(ctx, ref, learning.ActionRead, func(operation context.Context, _ learning.Lesson) error {
		_, revokeErr := repository.RevokeLesson(operation, learning.RevokeRequest{ID: request.ID, ExpectedRevision: 1, Actor: "reviewer", Reason: "withdrawn"})
		return revokeErr
	})
	require.ErrorIs(t, err, learning.ErrIneligible)
	metadata, err = guard.Review(ctx, request.ID)
	require.NoError(t, err)
	require.False(t, metadata.RevokedAt.IsZero())
	body, err = json.Marshal(metadata)
	require.NoError(t, err)
	require.NotContains(t, string(body), publication.Payload.Statement)
	authority.denied = true
	_, err = guard.Delete(ctx, learning.DeletionRequest{ID: request.ID, ExpectedRevision: 2, Actor: "retention-worker", Reason: "policy"})
	require.ErrorIs(t, err, learning.ErrDenied)
	authority.denied = false
	_, err = guard.Delete(ctx, learning.DeletionRequest{ID: request.ID, ExpectedRevision: 2, Actor: "retention-worker", Reason: "policy"})
	require.NoError(t, err)
}
