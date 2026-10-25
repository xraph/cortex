package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/store"
)

func testOrchestrationContinuation(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	journal, ok := s.(orchestration.ContinuationStore)
	require.True(t, ok)
	claims, ok := s.(orchestration.ContinuationRunStore)
	require.True(t, ok)
	ctx := ctxWithScope("continuation", "project")
	team := id.NewOrchestrationID()
	cfg := orchestration.Config{ID: id.NewOrchestrationConfigID(), Scope: cortex.ScopeFromContext(ctx), Strategy: orchestration.StrategySequential, Participants: []orchestration.Participant{{AgentName: "worker"}}}
	c := orchestration.Continuation{SchemaVersion: 1, OrchestrationID: team, Scope: cfg.Scope, Config: cfg, Input: "private input", Payload: []byte("private host snapshot")}
	require.NoError(t, journal.PutOrchestrationContinuation(ctx, c))
	require.NoError(t, journal.PutOrchestrationContinuation(ctx, c))
	changed := c
	changed.Input = "changed"
	require.ErrorIs(t, journal.PutOrchestrationContinuation(ctx, changed), cortex.ErrInvalidContinuation)
	read, err := journal.GetOrchestrationContinuation(ctx, team)
	require.NoError(t, err)
	require.Equal(t, c, read)
	_, err = journal.GetOrchestrationContinuation(ctxWithScope("continuation"), team)
	require.Error(t, err)
	require.ErrorIs(t, journal.PutOrchestrationContinuation(context.Background(), c), cortex.ErrNoScope)
	intent := orchestration.InvocationIntent{SchemaVersion: 1, OrchestrationID: team, Scope: c.Scope, Slot: "sequential/0", AgentName: "worker", RequestDigest: evidence.Digest([]byte("request"))}
	require.NoError(t, journal.BeginOrchestrationInvocation(ctx, intent))
	child := id.NewAgentRunID()
	result := orchestration.AgentResult{RunID: child, AgentName: "worker", Output: "result"}
	require.Error(t, journal.PutOrchestrationInvocationResult(ctx, intent, result))
	require.NoError(t, journal.BindOrchestrationInvocation(ctx, intent, child))
	absent, err := journal.GetOrchestrationInvocationResult(ctx, intent)
	require.NoError(t, err)
	require.Nil(t, absent)
	require.NoError(t, journal.PutOrchestrationInvocationResult(ctx, intent, result))
	require.NoError(t, journal.PutOrchestrationInvocationResult(ctx, intent, result))
	altered := result
	altered.Output = "changed"
	require.ErrorIs(t, journal.PutOrchestrationInvocationResult(ctx, intent, altered), cortex.ErrInvalidContinuation)
	saved, err := journal.GetOrchestrationInvocationResult(ctx, intent)
	require.NoError(t, err)
	require.Equal(t, result, *saved)
	parent := &orchestration.Run{ContinuationGeneration: 1, ID: team, ConfigID: cfg.ID, Status: orchestration.StatusRunning, Input: c.Input, Strategy: cfg.Strategy, StartedAt: time.Now().UTC()}
	require.NoError(t, s.CreateOrchestrationRun(ctx, parent))
	require.NoError(t, claims.LinkOrchestrationChild(ctx, team, child))
	parent.Status = orchestration.StatusWaiting
	require.NoError(t, s.UpdateOrchestrationRun(ctx, parent))
	stale := *parent
	stale.Status = orchestration.StatusRunning
	require.ErrorIs(t, s.UpdateOrchestrationRun(ctx, &stale), cortex.ErrOrchestrationRunConflict)
	_, err = claims.ClaimOrchestrationContinuation(ctxWithScope("continuation"), team)
	require.ErrorIs(t, err, cortex.ErrOrchestrationRunNotFound)
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { _, claimErr := claims.ClaimOrchestrationContinuation(ctx, team); results <- claimErr })
	}
	wg.Wait()
	close(results)
	winners := 0
	for claimErr := range results {
		if claimErr == nil {
			winners++
		} else {
			require.ErrorIs(t, claimErr, cortex.ErrOrchestrationRunConflict)
		}
	}
	require.Equal(t, 1, winners)
	require.ErrorIs(t, s.UpdateOrchestrationRun(ctx, parent), cortex.ErrOrchestrationRunConflict, "previous pass cannot restore waiting after another caller claims it")
	active, err := s.GetOrchestrationRun(ctx, team)
	require.NoError(t, err)
	require.Equal(t, orchestration.StatusRunning, active.Status)
	require.Equal(t, int64(2), active.ContinuationGeneration)
	for _, status := range []string{orchestration.StatusCompleted, orchestration.StatusFailed} {
		stale.Status = status
		require.ErrorIs(t, s.UpdateOrchestrationRun(ctx, &stale), cortex.ErrOrchestrationRunConflict)
	}
	require.Contains(t, active.AgentRunIDs, child)
	active.Status = orchestration.StatusCompleted
	require.NoError(t, s.UpdateOrchestrationRun(ctx, active))
	_, err = claims.ClaimOrchestrationContinuation(ctx, team)
	require.ErrorIs(t, err, cortex.ErrOrchestrationRunConflict)
}

// OrchestrationContinuationUpgrade preserves legacy teams without granting resume authority.
func OrchestrationContinuationUpgrade(t *testing.T, s store.Store, legacy func()) {
	t.Helper()
	ctx := ctxWithScope("continuation-upgrade")
	child := id.NewAgentRunID()
	parent := &orchestration.Run{ID: id.NewOrchestrationID(), Status: orchestration.StatusRunning, AgentRunIDs: []id.AgentRunID{child}, StartedAt: time.Now().UTC()}
	require.NoError(t, s.CreateOrchestrationRun(ctx, parent))
	legacy()
	require.NoError(t, s.Migrate(context.Background()))
	require.NoError(t, s.Migrate(context.Background()))
	retained, err := s.GetOrchestrationRun(ctx, parent.ID)
	require.NoError(t, err)
	require.Zero(t, retained.ContinuationGeneration)
	require.Equal(t, parent.AgentRunIDs, retained.AgentRunIDs)
	retained.Status = orchestration.StatusWaiting
	require.NoError(t, s.UpdateOrchestrationRun(ctx, retained))
	claims, ok := s.(orchestration.ContinuationRunStore)
	require.True(t, ok)
	_, err = claims.ClaimOrchestrationContinuation(ctx, parent.ID)
	require.ErrorIs(t, err, cortex.ErrOrchestrationRunConflict)
	retained.Status = orchestration.StatusFailed
	require.NoError(t, s.UpdateOrchestrationRun(ctx, retained))
}

// OrchestrationSQLContinuationUpgrade also replays a schema change whose acknowledgment was lost.
func OrchestrationSQLContinuationUpgrade(t *testing.T, s store.Store, exec func(string) error) {
	t.Helper()
	const forget = "DELETE FROM grove_migrations WHERE version = '20261025000004' AND \"group\" = 'cortex'"
	OrchestrationContinuationUpgrade(t, s, func() {
		require.NoError(t, exec("ALTER TABLE cortex_orchestration_runs DROP COLUMN continuation_generation"))
		require.NoError(t, exec(forget))
	})
	require.NoError(t, exec(forget))
	require.NoError(t, s.Migrate(context.Background()))
}
