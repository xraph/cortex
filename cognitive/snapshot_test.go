package cognitive_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/execution"
)

func capabilityInput() cognitive.SnapshotInput {
	return cognitive.SnapshotInput{ObservedAt: time.Now().UTC(), Budget: execution.Budget{MaxSteps: 4, MaxTokens: 100, MaxToolCalls: 3}, Usage: execution.Usage{Steps: 2, Tokens: 40, ReportedTokens: 35, ToolCalls: 1}, Tools: []cognitive.ToolObservation{{Name: "search", Kind: cognitive.ToolLocal, Installed: true, Visible: true}}}
}

func TestCapabilityRequiresInstalledAuthorizedTool(t *testing.T) {
	for _, tc := range []struct {
		name               string
		installed, visible bool
		want               cognitive.CapabilityState
	}{
		{"requestable", true, true, cognitive.CapabilityRequestable},
		{"missing", false, true, cognitive.CapabilityUnavailable},
		{"denied", true, false, cognitive.CapabilityDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := capabilityInput()
			in.Tools[0].Installed, in.Tools[0].Visible = tc.installed, tc.visible
			s, err := cognitive.BuildSnapshot(in)
			require.NoError(t, err)
			require.Equal(t, tc.want, s.Tools[0].State)
			require.Equal(t, cognitive.HealthUnknown, s.Tools[0].Health)
			require.Equal(t, "unknown", s.Reliability)
		})
	}
}

func TestCapabilityHealthDoesNotInventAvailability(t *testing.T) {
	in := capabilityInput()
	in.Tools[0].Health = cognitive.HealthUnavailable
	s, err := cognitive.BuildSnapshot(in)
	require.NoError(t, err)
	require.Equal(t, cognitive.CapabilityServiceUnavailable, s.Tools[0].State)
	in.Tools[0].Installed = false
	in.Tools[0].Health = cognitive.HealthHealthy
	s, err = cognitive.BuildSnapshot(in)
	require.NoError(t, err)
	require.Equal(t, cognitive.CapabilityUnavailable, s.Tools[0].State)
}

func TestCapabilitySnapshotRetainsLimitsWithoutGrantingBudget(t *testing.T) {
	in := capabilityInput()
	s, err := cognitive.BuildSnapshot(in)
	require.NoError(t, err)
	require.Equal(t, 2, *s.Remaining.Steps)
	require.Equal(t, 60, *s.Remaining.Tokens)
	require.Equal(t, 2, *s.Remaining.ToolCalls)
	require.Equal(t, in.Usage, s.Usage)
	in.Budget.MaxTokens = 0
	in.Usage.Steps = 10
	s, err = cognitive.BuildSnapshot(in)
	require.NoError(t, err)
	require.Nil(t, s.Remaining.Tokens, "disabled limit is not a zero allowance")
	require.Zero(t, *s.Remaining.Steps)
}

func TestCapabilitySnapshotRejectsMalformedObservations(t *testing.T) {
	for _, change := range []func(*cognitive.SnapshotInput){
		func(in *cognitive.SnapshotInput) { in.ObservedAt = time.Time{} },
		func(in *cognitive.SnapshotInput) { in.Tools[0].Name = "" },
		func(in *cognitive.SnapshotInput) { in.Tools[0].Kind = "claimed" },
		func(in *cognitive.SnapshotInput) { in.Tools[0].Health = "guaranteed" },
		func(in *cognitive.SnapshotInput) { in.Tools = append(in.Tools, in.Tools[0]) },
		func(in *cognitive.SnapshotInput) { in.Usage.ReportedTokens = 101 },
		func(in *cognitive.SnapshotInput) { in.Usage.ToolCalls = -1 },
		func(in *cognitive.SnapshotInput) { in.Budget.MaxTokens = -1 },
	} {
		in := capabilityInput()
		change(&in)
		s, err := cognitive.BuildSnapshot(in)
		require.ErrorIs(t, err, cognitive.ErrInvalidSnapshot)
		require.Zero(t, s)
	}
}

func TestCapabilitySnapshotIsDetachedAndDeterministic(t *testing.T) {
	in := capabilityInput()
	in.Tools = append(in.Tools, cognitive.ToolObservation{Name: "ask", Kind: cognitive.ToolExternal, Installed: true, Visible: true})
	s, err := cognitive.BuildSnapshot(in)
	require.NoError(t, err)
	require.Equal(t, "ask", s.Tools[0].Name)
	in.Tools[1].Name = "changed"
	require.Equal(t, "ask", s.Tools[0].Name)
}

func TestCapabilityExhaustedToolBudgetPreventsRequestability(t *testing.T) {
	in := capabilityInput()
	in.Usage.ToolCalls = in.Budget.MaxToolCalls
	s, err := cognitive.BuildSnapshot(in)
	require.NoError(t, err)
	require.Equal(t, cognitive.CapabilityState("budget_exhausted"), s.Tools[0].State)
}
