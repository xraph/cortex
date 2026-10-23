package verification_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/verification"
)

func TestResultContentTransportParsing(t *testing.T) {
	runID, resultID := id.NewAgentRunID(), id.New(verification.PrefixResult)
	request, err := verification.ParseResultContentRequest(runID.String(), resultID.String())
	require.NoError(t, err)
	require.Equal(t, runID, request.RunID)
	require.Equal(t, resultID, request.ResultID)
	for _, pair := range [][2]string{{"bad", resultID.String()}, {runID.String(), "SECRET"}, {id.NewAgentID().String(), resultID.String()}, {runID.String(), id.NewAgentID().String()}} {
		request, err = verification.ParseResultContentRequest(pair[0], pair[1])
		require.ErrorIs(t, err, verification.ErrInvalid)
		require.Zero(t, request)
		require.NotContains(t, err.Error(), "SECRET")
	}
}

func TestResultContentFailureVocabulary(t *testing.T) {
	for cause, code := range map[error]string{verification.ErrInvalid: "result_invalid", verification.ErrResultAccessDenied: "result_denied", cortex.ErrNoScope: "result_denied", verification.ErrNotFound: "result_not_found", verification.ErrConflict: "result_stale", safety.ErrBlocked: "result_blocked", safety.ErrLimit: "result_limit", context.DeadlineExceeded: "result_interrupted", errors.New("SECRET storage"): "result_unavailable"} {
		failure := verification.DescribeResultContentFailure(fmt.Errorf("SECRET provider: %w", cause))
		require.Equal(t, code, failure.Code)
		require.NotContains(t, failure.Message, "SECRET")
	}
}

func TestPublishedResultJSONPreservesLargeRevision(t *testing.T) {
	original := verification.PublishedResult{RunRevision: 9007199254740993}
	body, err := json.Marshal(verification.NewResultContentResponse(original))
	require.NoError(t, err)
	require.Contains(t, string(body), `"run_revision":"9007199254740993"`)
	var restored verification.ResultContentResponse
	require.NoError(t, json.Unmarshal(body, &restored))
	require.Equal(t, "9007199254740993", restored.RunRevision)
}
