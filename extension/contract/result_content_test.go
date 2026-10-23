package contract

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	dc "github.com/xraph/forge/extensions/dashboard/contract"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/resulttest"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/verification"
)

func TestResultContentContract(t *testing.T) {
	revoked := false
	d, s, st := testService(t, resulttest.Options(resulttest.Authorizer(func(ctx context.Context, _ verification.ResultContentRequest) error {
		require.NotNil(t, cortex.PrincipalFromContext(ctx))
		if revoked {
			return errors.New("SECRET grant")
		}
		return nil
	}), resulttest.Gate(func(context.Context, safety.PublicationRequest) (safety.PublicationDecision, error) {
		return safety.PublicationDecision{Text: "public projection", Status: safety.StatusRedacted}, nil
	}))...)
	p := principal("tenant-a")
	ctx, err := s.context(context.Background(), p, "read")
	require.NoError(t, err)
	receipt := resulttest.Commit(ctx, t, st)
	input := map[string]any{"id": receipt.Request.Current.RunID.String(), "result_id": receipt.Request.ResultID.String()}
	body, err := dispatch(t, d, p, "runs.result", input)
	require.NoError(t, err)
	require.NotContains(t, string(body), "SECRET")
	var result verification.ResultContentResponse
	require.NoError(t, json.Unmarshal(body, &result))
	require.Equal(t, "public projection", result.Content)
	require.True(t, result.ContentChanged)
	_, err = dispatch(t, d, principal("tenant-b"), "runs.result", input)
	var failure *dc.Error
	require.ErrorAs(t, err, &failure)
	require.Equal(t, dc.CodeNotFound, failure.Code)
	revoked = true
	_, err = dispatch(t, d, p, "runs.result", input)
	require.ErrorAs(t, err, &failure)
	require.Equal(t, dc.CodePermissionDenied, failure.Code)
	require.NotContains(t, err.Error(), "SECRET")
	revoked = false
	require.NoError(t, st.TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), receipt.Request.Current.Candidate, "deleted"))
	_, err = dispatch(t, d, p, "runs.result", input)
	require.ErrorAs(t, err, &failure)
	require.Equal(t, dc.CodeUnavailable, failure.Code)
}

func TestResultContentContractInvalidAndUnavailable(t *testing.T) {
	d, _, _ := testService(t)
	for _, tc := range []struct {
		runID, resultID string
		code            dc.ErrorCode
	}{{"bad", id.New(verification.PrefixResult).String(), dc.CodeBadRequest}, {id.NewAgentRunID().String(), id.NewAgentID().String(), dc.CodeBadRequest}, {id.NewAgentRunID().String(), id.New(verification.PrefixResult).String(), dc.CodeUnavailable}} {
		_, err := dispatch(t, d, principal("tenant-a"), "runs.result", map[string]any{"id": tc.runID, "result_id": tc.resultID})
		var failure *dc.Error
		require.ErrorAs(t, err, &failure)
		require.Equal(t, tc.code, failure.Code)
	}
}

func TestHistoricalDashboardReadsWithholdCommittedContent(t *testing.T) {
	d, s, st := testService(t)
	p := principal("private-results")
	ctx, err := s.context(t.Context(), p, "read")
	require.NoError(t, err)
	receipt := resulttest.Commit(ctx, t, st)
	current, err := st.GetRun(ctx, receipt.Request.Current.RunID)
	require.NoError(t, err)
	step := &run.Step{ID: id.NewStepID(), RunID: current.ID, Output: "SECRET step", Input: "SECRET input"}
	require.NoError(t, st.CreateStep(ctx, step))
	for _, check := range []struct {
		intent string
		input  any
	}{
		{"runs.detail", IDInput{ID: current.ID.String()}},
		{"runs.list", RunListInput{}},
		{"agents.stats", IDInput{ID: current.AgentID.String()}},
	} {
		body, dispatchErr := dispatch(t, d, p, check.intent, check.input)
		require.NoError(t, dispatchErr)
		require.NotContains(t, string(body), "SECRET")
		require.Contains(t, string(body), current.ResultRef)
		require.Contains(t, string(body), "content_withheld")
	}
	saved, err := st.GetRun(ctx, current.ID)
	require.NoError(t, err)
	require.Equal(t, "SECRET candidate", saved.Output)
}

func TestProtectedRunRejectsCachedContentReplay(t *testing.T) {
	d, s, st := testService(t)
	p := principal("cached-content")
	ctx, err := s.context(t.Context(), p, "read")
	require.NoError(t, err)
	receipt := resulttest.Commit(ctx, t, st)
	runID := receipt.Request.Current.RunID.String()
	s.streams = map[string]*liveRun{runID: {events: []engine.StreamEvent{{Data: map[string]any{"output": "SECRET cached output"}}}, done: true}}
	body, err := dispatch(t, d, p, "runs.events", StreamInput{ID: runID})
	require.Error(t, err)
	require.NotContains(t, string(body), "SECRET")
	var failure *dc.Error
	require.ErrorAs(t, err, &failure)
	require.Equal(t, dc.CodeConflict, failure.Code)
	require.Equal(t, "protected_replay_requires_guarded_reads", failure.Details["code"])
}
