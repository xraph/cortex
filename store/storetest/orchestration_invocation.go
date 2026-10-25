package storetest

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/store"
)

func testOrchestrationInvocationJournal(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	journal, ok := backend.(orchestration.InvocationStore)
	require.True(t, ok)
	ctx := ctxWithScope("invocation", "project")
	intent := orchestration.InvocationIntent{SchemaVersion: 1, OrchestrationID: id.NewOrchestrationID(), Scope: cortex.ScopeFromContext(ctx), Slot: "debate/round/0/participant/0", AgentName: "reviewer", RequestDigest: evidence.Digest([]byte("private input and options"))}
	require.ErrorIs(t, journal.BeginOrchestrationInvocation(context.Background(), intent), cortex.ErrNoScope)
	require.ErrorIs(t, journal.BeginOrchestrationInvocation(ctxWithScope("invocation"), intent), cortex.ErrInvalidScope)
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { results <- journal.BeginOrchestrationInvocation(ctx, intent) })
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else {
			require.ErrorIs(t, err, cortex.ErrAlreadyExists)
		}
	}
	require.Equal(t, 1, winners)
	unbound, err := journal.GetOrchestrationInvocation(ctx, intent.OrchestrationID, intent.Slot)
	require.NoError(t, err)
	require.True(t, unbound.RunID.IsNil())
	require.Equal(t, intent, unbound.Intent)
	changed := intent
	changed.RequestDigest = evidence.Digest([]byte("different"))
	require.ErrorIs(t, journal.BeginOrchestrationInvocation(ctx, changed), cortex.ErrAlreadyExists)
	require.ErrorIs(t, journal.BindOrchestrationInvocation(ctx, changed, id.NewAgentRunID()), cortex.ErrInvalidState)
	require.ErrorIs(t, journal.BindOrchestrationInvocation(ctx, intent, id.NewAgentID()), cortex.ErrInvalidState)
	children := []id.AgentRunID{id.NewAgentRunID(), id.NewAgentRunID()}
	results = make(chan error, 2)
	for _, child := range children {
		wg.Go(func() { results <- journal.BindOrchestrationInvocation(ctx, intent, child) })
	}
	wg.Wait()
	close(results)
	winners = 0
	for err := range results {
		if err == nil {
			winners++
		} else {
			require.ErrorIs(t, err, cortex.ErrAlreadyExists)
		}
	}
	require.Equal(t, 1, winners)
	retained, err := journal.GetOrchestrationInvocation(ctx, intent.OrchestrationID, intent.Slot)
	require.NoError(t, err)
	require.Contains(t, children, retained.RunID)
	require.ErrorIs(t, journal.BindOrchestrationInvocation(ctx, intent, retained.RunID), cortex.ErrAlreadyExists)
	for _, other := range []context.Context{ctxWithScope("invocation"), ctxWithScope("invocation", "other"), ctxWithScope("unrelated")} {
		_, err = journal.GetOrchestrationInvocation(other, intent.OrchestrationID, intent.Slot)
		require.ErrorIs(t, err, cortex.ErrOrchestrationInvocationNotFound)
	}
	// A new slot is distinct even when the same participant and request repeat.
	second := intent
	second.Slot = "debate/round/1/participant/0"
	require.NoError(t, journal.BeginOrchestrationInvocation(ctx, second))
	original, err := journal.GetOrchestrationInvocation(ctx, intent.OrchestrationID, intent.Slot)
	require.NoError(t, err)
	require.Equal(t, retained, original)
}
