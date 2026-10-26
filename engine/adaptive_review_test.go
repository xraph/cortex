package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
)

type adaptiveAuthority func(context.Context, engine.AdaptiveReviewResource) error

func (a adaptiveAuthority) AuthorizeAdaptiveReview(ctx context.Context, resource engine.AdaptiveReviewResource) error {
	return a(ctx, resource)
}

func TestAdaptiveReviewHistoricalDisclosure(t *testing.T) {
	backend := newSQLiteStoreForTest(t)
	ctx := overlayCtx("review")
	now := time.Now().UTC()
	artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: cortex.ScopeFromContext(ctx), MediaType: "application/json", Version: "v1", Producer: "host", Sensitivity: evidence.SensitivityRestricted, CreatedAt: now.Add(-time.Second), RetainUntil: now.Add(time.Hour)}, Data: []byte(`{"ready":false}`)})
	require.NoError(t, err)
	require.NoError(t, backend.SaveEvidence(ctx, artifact))
	host := &reflectionHost{observation: func(in engine.ReflectionObservationInput) cognitive.ReflectionSnapshot {
		s := observation(in)
		s.Assumptions = []cognitive.Assumption{{Key: "ready", Summary: "Private readiness claim", Origin: cognitive.AssumptionHost, State: cognitive.AssumptionDisputed, Evidence: []evidence.Ref{artifact.Ref}}}
		return s
	}}
	e, err := engine.New(engine.WithStore(backend), engine.WithLLM(&requiredClient{response: &llm.Response{Content: "unused", FinishReason: "stop"}}), engine.WithReflection(engine.ReflectionConfig{Policy: cognitive.ReflectionPolicy{Revision: "v1"}, Observer: host, Fence: host, Authorizer: host}))
	require.NoError(t, err)
	ag := &agent.Config{Entity: cortex.NewEntity(), ID: id.NewAgentID(), Name: "review-worker"}
	require.NoError(t, backend.Create(ctx, ag))
	budget := execution.Budget{MaxSteps: 5, MaxTokens: 100}
	prepared, err := e.PrepareAgent(ctx, ag.ID, &engine.RunOverrides{Budget: &budget})
	require.NoError(t, err)
	result, err := e.RunPrepared(ctx, prepared, "work", engine.PreparedRunOptions{})
	require.NoError(t, err)
	denied, reads := false, 0
	revokeDuringRead := false
	authority := adaptiveAuthority(func(_ context.Context, resource engine.AdaptiveReviewResource) error {
		require.Equal(t, result.ID, resource.RunID)
		if denied {
			return evidence.ErrDenied
		}
		return nil
	})
	repository, err := evidence.NewRepository(backend, captureAuthorizer(func(context.Context, evidence.Action, evidence.Ref) error {
		reads++
		if revokeDuringRead {
			denied = true
		}
		return nil
	}), 1<<20)
	require.NoError(t, err)
	reviewer, err := engine.NewAdaptiveReviewer(engine.AdaptiveReviewConfig{Store: backend, Evidence: repository, Authority: authority})
	require.NoError(t, err)
	page, err := reviewer.ReviewRun(ctx, result.ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Len(t, page.Items[0].Assumptions, 1)
	require.True(t, page.Items[0].EvidenceAvailable)
	require.Equal(t, "unknown", page.Reliability.Status)
	require.Positive(t, reads)
	revokeDuringRead = true
	page, err = reviewer.ReviewRun(ctx, result.ID, 0, 10)
	require.ErrorIs(t, err, evidence.ErrDenied)
	require.Empty(t, page.Items)
	revokeDuringRead = false
	// The reviewer has no dependency on an enabled Engine or running worker.
	denied = true
	page, err = reviewer.ReviewRun(ctx, result.ID, 0, 10)
	require.ErrorIs(t, err, evidence.ErrDenied)
	require.Empty(t, page.Items)
	denied = false
	_, err = reviewer.ReviewRun(overlayCtx("other"), result.ID, 0, 10)
	require.Error(t, err)
	require.NoError(t, backend.TombstoneEvidence(evidence.WithDeletionActor(ctx, "retention"), artifact.Ref, "expired"))
	page, err = reviewer.ReviewRun(ctx, result.ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.False(t, page.Items[0].EvidenceAvailable)
	require.Empty(t, page.Items[0].Assumptions)
	require.Equal(t, cognitive.ReflectionConflict, page.Items[0].Decision.Reason)
	_, err = reviewer.ReviewRun(ctx, result.ID, 0, 101)
	require.Error(t, err)
	_, err = engine.NewAdaptiveReviewer(engine.AdaptiveReviewConfig{})
	require.Error(t, err)
	if errors.Is(err, cortex.ErrRunNotFound) {
		t.Fatal("configuration error disguised as missing run")
	}
}
