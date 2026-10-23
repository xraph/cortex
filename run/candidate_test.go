package run_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

func TestCandidateCaptureFreeze(t *testing.T) {
	request := run.CandidateCapture{SchemaVersion: 1, IntentID: id.New(verification.PrefixCaptureIntent), ExpectedRunRevision: 1, Output: "private answer", FinishReason: "stop", Usage: execution.Usage{Steps: 1, Tokens: 10, ReportedTokens: 10}}
	frozen, digest, err := request.Freeze()
	require.NoError(t, err)
	require.Equal(t, request, frozen)
	_, retry, err := frozen.Freeze()
	require.NoError(t, err)
	require.Equal(t, digest, retry)
	changed := request
	changed.Output += " changed"
	_, different, err := changed.Freeze()
	require.NoError(t, err)
	require.NotEqual(t, digest, different)
	for _, mutate := range []func(*run.CandidateCapture){
		func(r *run.CandidateCapture) { r.SchemaVersion++ },
		func(r *run.CandidateCapture) { r.IntentID = id.NewAgentRunID() },
		func(r *run.CandidateCapture) { r.ExpectedRunRevision = 0 },
		func(r *run.CandidateCapture) { r.ExpectedRunRevision = math.MaxInt64 - 2 },
		func(r *run.CandidateCapture) { r.Output = string([]byte{0xff}) },
		func(r *run.CandidateCapture) { r.FinishReason = "length" },
		func(r *run.CandidateCapture) { r.Usage.Steps = -1 },
		func(r *run.CandidateCapture) { r.Usage.Tokens = -1 },
		func(r *run.CandidateCapture) { r.Usage.ReportedTokens = -1 },
		func(r *run.CandidateCapture) { r.Usage.ReportedTokens = 11 },
		func(r *run.CandidateCapture) { r.Usage.ToolCalls = -1 },
	} {
		invalid := request
		mutate(&invalid)
		_, _, err = invalid.Freeze()
		require.ErrorIs(t, err, verification.ErrInvalid)
	}
}
