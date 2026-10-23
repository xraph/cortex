package event_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

func terminalRequest() event.TerminalUpdate {
	now := time.Now().UTC()
	return event.TerminalUpdate{SchemaVersion: 1, EventID: id.New(event.PrefixEvent), Run: run.Run{ID: id.NewAgentRunID(), AgentID: id.NewAgentID(), Scope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "a"}}}, Revision: 1, State: run.StateCompleted, CompletedAt: &now, VerificationStatus: verification.StatusUnchecked, Metadata: map[string]any{"nested": map[string]any{"value": "original"}}}}
}
func TestTerminalUpdateFreezesSnapshot(t *testing.T) {
	request := terminalRequest()
	frozen, digest, err := request.Freeze()
	require.NoError(t, err)
	request.Run.Metadata["nested"].(map[string]any)["value"] = "changed"
	require.Equal(t, "original", frozen.Run.Metadata["nested"].(map[string]any)["value"])
	_, again, err := frozen.Freeze()
	require.NoError(t, err)
	require.Equal(t, digest, again)
	_, changed, err := request.Freeze()
	require.NoError(t, err)
	require.NotEqual(t, digest, changed)
}
func TestTerminalUpdateRejectsNonterminalAndVerificationBypass(t *testing.T) {
	for _, mutate := range []func(*event.TerminalUpdate){
		func(r *event.TerminalUpdate) { r.Run.State = run.StateRunning },
		func(r *event.TerminalUpdate) { r.Run.Provenance.VerificationRequired = true },
		func(r *event.TerminalUpdate) { r.Run.VerificationStatus = verification.StatusPassed },
		func(r *event.TerminalUpdate) { r.Run.ResultRef = id.New(verification.PrefixResult).String() },
		func(r *event.TerminalUpdate) { r.EventID = id.NewAgentRunID() },
		func(r *event.TerminalUpdate) { r.Run.CompletedAt = nil },
	} {
		request := terminalRequest()
		mutate(&request)
		_, _, err := request.Freeze()
		require.Error(t, err)
	}
}

func TestTerminalUpdatePreservesMetadataIntegerPrecision(t *testing.T) {
	request := terminalRequest()
	request.Run.Metadata["sequence"] = int64(9007199254740993)
	frozen, digest, err := request.Freeze()
	require.NoError(t, err)
	_, again, err := frozen.Freeze()
	require.NoError(t, err)
	require.Equal(t, digest, again)
}
