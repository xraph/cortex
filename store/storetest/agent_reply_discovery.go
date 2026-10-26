package storetest

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/suspension"
)

func testAgentReplyDiscovery(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	discovery, ok := backend.(suspension.ReplyDiscoveryStore)
	require.True(t, ok)
	ctx, durable, req := admissionFixture(t, backend)
	current, paused := mustSuspendRun(t, backend, ctx, nil)
	require.NoError(t, backend.DeleteSuspension(ctx, current.ID))
	paused.Reason = suspension.ReasonAgentReply
	require.NoError(t, backend.CreateSuspension(ctx, paused))
	_, err := durable.AdmitResume(ctx, req)
	require.NoError(t, err)
	// Both paused and already-admitted runs remain discoverable, without input.
	want := []id.AgentRunID{current.ID, req.Version.RunID}
	slices.SortFunc(want, func(a, b id.AgentRunID) int {
		if a.String() < b.String() {
			return -1
		}
		return 1
	})
	_, _ = mustSuspendRun(t, backend, ctx, nil) // External-tool pause is excluded.
	childScope := cortex.ScopeFromContext(ctx)
	childScope.Levels = append(slices.Clone(childScope.Levels), cortex.Level{Key: "project", Value: "child"})
	child := cortex.WithScope(ctx, childScope)
	foreignRun, foreignPause := mustSuspendRun(t, backend, child, nil)
	require.NoError(t, backend.DeleteSuspension(child, foreignRun.ID))
	foreignPause.Reason = suspension.ReasonAgentReply
	require.NoError(t, backend.CreateSuspension(child, foreignPause))
	page, err := discovery.ListAgentReplyRuns(ctx, id.Nil, 1)
	require.NoError(t, err)
	require.Equal(t, want[:1], page)
	page, err = discovery.ListAgentReplyRuns(ctx, page[0], 1)
	require.NoError(t, err)
	require.Equal(t, want[1:], page)
	page, err = discovery.ListAgentReplyRuns(ctx, page[0], 1)
	require.NoError(t, err)
	require.Empty(t, page)
	page, err = discovery.ListAgentReplyRuns(child, id.Nil, 256)
	require.NoError(t, err)
	require.Equal(t, []id.AgentRunID{foreignRun.ID}, page)
	_, err = discovery.ListAgentReplyRuns(context.Background(), id.Nil, 1)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	for _, limit := range []int{0, -1, 257} {
		_, err = discovery.ListAgentReplyRuns(ctx, id.Nil, limit)
		require.Error(t, err)
	}
	_, err = discovery.ListAgentReplyRuns(ctx, id.NewSuspensionID(), 1)
	require.Error(t, err)
}
