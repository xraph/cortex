package sqlite

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func TestOrchestrationContinuationReopen(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "continuation.db")
	s := openTestStore(t, dsn)
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "reopen"}}}
	ctx := cortex.WithScope(t.Context(), scope)
	team, child := id.NewOrchestrationID(), id.NewAgentRunID()
	c := orchestration.Continuation{SchemaVersion: 1, OrchestrationID: team, Scope: scope, Config: orchestration.Config{Scope: scope, Strategy: orchestration.StrategySequential, Participants: []orchestration.Participant{{AgentName: "worker"}}}, Input: "private", Payload: []byte("host snapshot")}
	require.NoError(t, s.PutOrchestrationContinuation(ctx, c))
	parent := &orchestration.Run{ContinuationGeneration: 1, ID: team, Status: orchestration.StatusRunning, Strategy: c.Config.Strategy, Input: c.Input, StartedAt: time.Now().UTC()}
	require.NoError(t, s.CreateOrchestrationRun(ctx, parent))
	require.NoError(t, s.LinkOrchestrationChild(ctx, team, child))
	in := orchestration.InvocationIntent{SchemaVersion: 1, OrchestrationID: team, Scope: scope, Slot: "sequential/0", AgentName: "worker", RequestDigest: evidence.Digest([]byte("request"))}
	require.NoError(t, s.BeginOrchestrationInvocation(ctx, in))
	require.NoError(t, s.BindOrchestrationInvocation(ctx, in, child))
	result := orchestration.AgentResult{RunID: child, AgentName: "worker", Output: "saved output"}
	require.NoError(t, s.PutOrchestrationInvocationResult(ctx, in, result))
	parent.Status = orchestration.StatusWaiting
	require.NoError(t, s.UpdateOrchestrationRun(ctx, parent))
	require.NoError(t, s.Close())
	s = openTestStore(t, dsn)
	saved, err := s.GetOrchestrationContinuation(ctx, team)
	require.NoError(t, err)
	require.Equal(t, c, saved)
	outcome, err := s.GetOrchestrationInvocationResult(ctx, in)
	require.NoError(t, err)
	require.Equal(t, result, *outcome)
	claimed, err := s.ClaimOrchestrationContinuation(ctx, team)
	require.NoError(t, err)
	require.Equal(t, orchestration.StatusRunning, claimed.Status)
	require.Contains(t, claimed.AgentRunIDs, child)
	_, err = s.ClaimOrchestrationContinuation(ctx, team)
	require.ErrorIs(t, err, cortex.ErrOrchestrationRunConflict)
}
