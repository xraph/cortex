package contract

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	"github.com/xraph/cortex/agent"
)

func TestAgentMutationFenceProtectsDashboardWrites(t *testing.T) {
	_, s, _ := testService(t)
	allowed, calls := false, 0
	s.deps.AgentMutationFence = func(ctx context.Context, apply func(context.Context) error) error {
		calls++
		if !allowed {
			return denied("revoked")
		}
		return apply(ctx)
	}
	d := dispatcher.New(nil)
	require.NoError(t, Register(d, dc.NewRegistry(), dc.NewWardenRegistry(), s.deps))
	p := principal("mutation")
	_, err := dispatch(t, d, p, "agents.create", CreateInput[agent.Config]{Data: agent.Config{Name: "guarded", Enabled: true}})
	require.ErrorIs(t, err, dc.ErrPermissionDenied)
	allowed = true
	raw, err := dispatch(t, d, p, "agents.create", CreateInput[agent.Config]{Data: agent.Config{Name: "guarded", Enabled: true}})
	require.NoError(t, err)
	var created agent.Config
	require.NoError(t, json.Unmarshal(raw, &created))
	allowed = false
	_, err = dispatch(t, d, p, "agents.update", map[string]any{"id": created.ID.String(), "patch": map[string]any{"description": "changed"}})
	require.ErrorIs(t, err, dc.ErrPermissionDenied)
	_, err = dispatch(t, d, p, "agents.delete", IDInput{ID: created.ID.String()})
	require.ErrorIs(t, err, dc.ErrPermissionDenied)
	require.Equal(t, 4, calls)
	ctx, err := s.context(context.Background(), p, "read")
	require.NoError(t, err)
	stored, err := s.deps.Engine.Store().Get(ctx, created.ID)
	require.NoError(t, err)
	require.Empty(t, stored.Description)
}
