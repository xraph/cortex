package storetest

import (
	"context"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
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
	admitted, err := durable.AdmitResume(ctx, req)
	require.NoError(t, err)
	// A start can outlive its deleted pause without becoming safe to replay.
	_, err = durable.StartResume(ctx, req.Version.RunID, req.Version.SuspensionID, admitted.RequestDigest)
	require.NoError(t, err)
	// The same run in both projections must consume only one cursor slot.
	overlap, overlapErr := discovery.ListAgentReplyRuns(ctx, id.Nil, 256)
	require.NoError(t, overlapErr)
	require.Len(t, overlap, 2)
	lookup, supported := backend.(suspension.ResumeRecoveryStore)
	require.True(t, supported)
	latest, lookupErr := lookup.LatestResumeAdmission(ctx, req.Version.RunID)
	require.NoError(t, lookupErr)
	require.Equal(t, admitted.RequestDigest, latest.RequestDigest)
	_, lookupErr = lookup.LatestResumeAdmission(ctxWithScope("other"), req.Version.RunID)
	require.ErrorIs(t, lookupErr, suspension.ErrResumeAdmissionNotFound)
	require.NoError(t, backend.DeleteSuspension(ctx, req.Version.RunID))
	// Both paused and started runs remain discoverable, without input.
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
	finished, finishErr := backend.GetRun(ctx, req.Version.RunID)
	require.NoError(t, finishErr)
	finished.State = run.StateIncomplete
	require.NoError(t, backend.UpdateRun(ctx, finished))
	remaining, remainingErr := discovery.ListAgentReplyRuns(ctx, id.Nil, 256)
	require.NoError(t, remainingErr)
	require.Equal(t, []id.AgentRunID{current.ID}, remaining)
	_, err = discovery.ListAgentReplyRuns(context.Background(), id.Nil, 1)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	for _, limit := range []int{0, -1, 257} {
		_, err = discovery.ListAgentReplyRuns(ctx, id.Nil, limit)
		require.Error(t, err)
	}
	_, err = discovery.ListAgentReplyRuns(ctx, id.NewSuspensionID(), 1)
	require.Error(t, err)
}
