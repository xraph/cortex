package execution_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

func runCallRequest() execution.RunCallRequest {
	return execution.RunCallRequest{SchemaVersion: 1, ID: id.New(execution.PrefixRunCall), Scope: cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "one"}}}, RunID: id.NewAgentRunID(), AgentID: id.NewAgentID(), ClaimID: id.New("tclaim"), ParentRunID: id.NewAgentRunID(), Kind: execution.RunCallComplete, OperationDigest: evidence.Digest([]byte("generation:0")), RequestDigest: evidence.Digest([]byte("exact leased request")), PreparedDigest: evidence.Digest([]byte("prepared")), AssignmentDigest: evidence.Digest([]byte("handoff")), Deadline: time.Now().UTC().Add(time.Minute)}
}

func TestRunCallRequestBindsStableOperation(t *testing.T) {
	r := runCallRequest()
	frozen, digest, err := r.Freeze()
	require.NoError(t, err)
	require.True(t, evidence.ValidDigest(digest))
	r.Scope.Levels[0].Value = "mutated"
	require.NotEqual(t, r.Scope, frozen.Scope)
	retry := frozen
	retry.ID = id.New(execution.PrefixRunCall)
	require.Equal(t, frozen.Key(), retry.Key(), "a new attempt cannot create a second logical call slot")
	_, nextDigest, err := retry.Freeze()
	require.NoError(t, err)
	require.NotEqual(t, digest, nextDigest)
	retry.RunID = id.NewAgentRunID()
	require.NotEqual(t, frozen.Key(), retry.Key())
}

func TestRunCallRejectsMalformedBindings(t *testing.T) {
	for name, change := range map[string]func(*execution.RunCallRequest){
		"scope":     func(r *execution.RunCallRequest) { r.Scope = cortex.Scope{} },
		"call":      func(r *execution.RunCallRequest) { r.ID = id.NewAgentID() },
		"claim":     func(r *execution.RunCallRequest) { r.ClaimID = id.NewAgentRunID() },
		"parent":    func(r *execution.RunCallRequest) { r.ParentRunID = r.RunID },
		"kind":      func(r *execution.RunCallRequest) { r.Kind = "unknown" },
		"operation": func(r *execution.RunCallRequest) { r.OperationDigest = "" },
		"request":   func(r *execution.RunCallRequest) { r.RequestDigest = "" },
		"deadline":  func(r *execution.RunCallRequest) { r.Deadline = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			r := runCallRequest()
			change(&r)
			_, _, err := r.Freeze()
			require.ErrorIs(t, err, execution.ErrInvalidRunCall)
		})
	}
}

func TestRunCallCompletionRetainsReportedUsageWithoutCertifyingIt(t *testing.T) {
	r := runCallRequest()
	_, digest, err := r.Freeze()
	require.NoError(t, err)
	c := execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: digest, ResultDigest: evidence.Digest([]byte("private output")), ReportedTokens: 7, TokensReported: true}
	require.NoError(t, c.Validate(r))
	c.TokensReported = false
	require.ErrorIs(t, c.Validate(r), execution.ErrInvalidRunCall)
	c.ReportedTokens = 0
	require.NoError(t, c.Validate(r), "unavailable usage stays unknown")
	c.RequestDigest = evidence.Digest(nil)
	require.ErrorIs(t, c.Validate(r), execution.ErrInvalidRunCall)
}

func TestRunCallContextDetachesScope(t *testing.T) {
	r := runCallRequest()
	ctx := execution.WithRunCallContext(context.Background(), r)
	r.Scope.Levels[0].Value = "caller mutation"
	got, ok := execution.RunCallFromContext(ctx)
	require.True(t, ok)
	require.NotEqual(t, r.Scope, got.Scope)
	got.Scope.Levels[0].Value = "provider mutation"
	again, ok := execution.RunCallFromContext(ctx)
	require.True(t, ok)
	require.NotEqual(t, got.Scope, again.Scope)
	_, ok = execution.RunCallFromContext(context.Background())
	require.False(t, ok)
}
