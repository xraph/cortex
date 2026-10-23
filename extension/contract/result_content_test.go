package contract

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	dc "github.com/xraph/forge/extensions/dashboard/contract"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/resulttest"
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
