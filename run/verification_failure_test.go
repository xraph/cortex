package run_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

func TestVerificationFailureContainsOnlyRecoveryMetadata(t *testing.T) {
	r := &run.Run{ID: id.NewAgentRunID(), State: run.StateCompleted, VerificationStatus: verification.StatusFailed, Input: "SECRET task", Output: "SECRET candidate"}
	for cause, code := range map[error]string{cortex.ErrRunUnverified: "run_unverified", cortex.ErrVerificationUnavailable: "verification_unavailable"} {
		failure, ok := run.DescribeVerificationFailure(fmt.Errorf("SECRET diagnostic: %w", cause), r)
		require.True(t, ok)
		require.Equal(t, code, failure.Code)
		require.Equal(t, cause.Error(), failure.Message)
		require.Equal(t, r.ID.String(), failure.RunID)
		require.False(t, failure.Retryable)
		data, err := json.Marshal(failure.Fields())
		require.NoError(t, err)
		require.NotContains(t, string(data), "SECRET")
		absent, ok := run.DescribeVerificationFailure(cause, nil)
		require.True(t, ok)
		require.NotContains(t, absent.Fields(), "run_id")
	}
	for _, cause := range []error{nil, errors.New("unknown")} {
		_, ok := run.DescribeVerificationFailure(cause, r)
		require.False(t, ok)
	}
}
