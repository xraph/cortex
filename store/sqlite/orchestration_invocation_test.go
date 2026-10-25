package sqlite

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func TestOrchestrationInvocationReopen(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "invocations.db")
	s := openTestStore(t, dsn)
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "journal"}}}
	ctx := cortex.WithScope(t.Context(), scope)
	intent := orchestration.InvocationIntent{SchemaVersion: 1, OrchestrationID: id.NewOrchestrationID(), Scope: scope, Slot: "sequential/0", AgentName: "worker", RequestDigest: evidence.Digest([]byte("private request"))}
	require.NoError(t, s.BeginOrchestrationInvocation(ctx, intent))
	require.NoError(t, s.Close())
	s = openTestStore(t, dsn)
	require.ErrorIs(t, s.BeginOrchestrationInvocation(ctx, intent), cortex.ErrAlreadyExists)
	unbound, err := s.GetOrchestrationInvocation(ctx, intent.OrchestrationID, intent.Slot)
	require.NoError(t, err)
	require.True(t, unbound.RunID.IsNil())
	child := id.NewAgentRunID()
	require.NoError(t, s.BindOrchestrationInvocation(ctx, intent, child))
	require.NoError(t, s.Close())
	s = openTestStore(t, dsn)
	bound, err := s.GetOrchestrationInvocation(ctx, intent.OrchestrationID, intent.Slot)
	require.NoError(t, err)
	require.Equal(t, child, bound.RunID)
	require.ErrorIs(t, s.BindOrchestrationInvocation(ctx, intent, id.NewAgentRunID()), cortex.ErrAlreadyExists)
}
