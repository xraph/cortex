package contract

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/learning"
	"github.com/xraph/cortex/store/storetest"
	"github.com/xraph/cortex/verification"
)

type adaptiveLessonAuthority struct{ denied bool }

func (a *adaptiveLessonAuthority) AuthorizeLesson(context.Context, learning.Action, learning.Resource) error {
	if a.denied {
		return learning.ErrDenied
	}
	return nil
}
func (a *adaptiveLessonAuthority) AuthorizeResultContent(context.Context, verification.ResultContentRequest) error {
	if a.denied {
		return learning.ErrDenied
	}
	return nil
}
func (*adaptiveLessonAuthority) WithCommitFence(_ context.Context, binding verification.Binding, action func(verification.Binding) error) error {
	return action(binding)
}
func (*adaptiveLessonAuthority) Now() time.Time { return time.Now().UTC() }

func TestAdaptiveUnavailableIsNotEmptyOrDenied(t *testing.T) {
	d, _, _ := testService(t)
	for _, intent := range []string{"cognitive.review", "learning.publications"} {
		raw, err := dispatch(t, d, principal("adaptive"), intent, map[string]any{})
		require.NoError(t, err)
		out := decode[struct {
			Available bool   `json:"available"`
			Reason    string `json:"reason"`
		}](t, raw)
		require.False(t, out.Available)
		require.NotEmpty(t, out.Reason)
		_, err = dispatch(t, d, dc.Principal{}, intent, map[string]any{})
		require.ErrorIs(t, err, dc.ErrUnauthenticated)
	}
}

func TestAdaptiveLearningCommandsUseCurrentAuthorityAndSafeViews(t *testing.T) {
	_, service, backend := testService(t)
	ctx, request := storetest.AcceptedLessonFixture(t, backend)
	authority := &adaptiveLessonAuthority{}
	guard, err := learning.NewGuard(learning.GuardConfig{Repository: backend, Results: backend, ContentAuthority: authority, Authority: authority, Fence: authority, Clock: authority})
	require.NoError(t, err)
	deps := service.deps
	deps.Learning = guard
	audits := []AuditEvent{}
	deps.Audit = func(_ context.Context, event AuditEvent) error { audits = append(audits, event); return nil }
	d := dispatcher.New(nil)
	require.NoError(t, Register(d, dc.NewRegistry(), dc.NewWardenRegistry(), deps))
	p := principal("unused")
	p.Claims = map[string]any{"cortex_scope": cortex.ScopeFromContext(ctx)}
	in := learningAcceptInput{ID: request.ID.String(), RunID: request.Result.RunID.String(), ResultID: request.Result.ResultID.String(), Domain: request.Domain, ExpiresAt: request.ExpiresAt}
	raw, err := dispatch(t, d, p, "learning.accept", in)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "private candidate text")
	require.Len(t, audits, 2)
	require.Equal(t, "succeeded", audits[1].Outcome)
	raw, err = dispatch(t, d, p, "learning.publications", learningPageInput{Limit: 10})
	require.NoError(t, err)
	page := decode[learningPageOutput](t, raw)
	require.True(t, page.Available)
	require.Len(t, page.Items, 1)
	require.NotContains(t, string(raw), "private candidate text")
	authority.denied = true
	_, err = dispatch(t, d, p, "learning.detail", IDInput{ID: in.ID})
	require.ErrorIs(t, err, dc.ErrPermissionDenied)
	authority.denied = false
	reader := p
	reader.User = principal("unused").User
	reader.User.Scopes = []string{"cortex.read"}
	_, err = dispatch(t, d, reader, "learning.revoke", learningRevokeInput{ID: in.ID, Revision: 1, Reason: "withdrawn"})
	require.ErrorIs(t, err, dc.ErrPermissionDenied)
	_, err = dispatch(t, d, p, "learning.revoke", learningRevokeInput{ID: in.ID, Revision: 1, Reason: "withdrawn"})
	require.NoError(t, err)
	raw, err = dispatch(t, d, p, "learning.detail", IDInput{ID: in.ID})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "private candidate text")
	review := decode[learning.LessonSummary](t, raw)
	require.False(t, review.RevokedAt.IsZero())
	_, err = dispatch(t, d, principal("foreign"), "learning.detail", IDInput{ID: in.ID})
	require.ErrorIs(t, err, dc.ErrNotFound)
}
