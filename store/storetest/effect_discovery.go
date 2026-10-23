package storetest

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

func testEffectDiscovery(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	s, ok := backend.(execution.EffectDiscoveryStore)
	require.True(t, ok, "effect discovery capability missing")
	writes, ok := backend.(execution.EffectStore)
	require.True(t, ok, "effect persistence capability missing")
	first := storedEffect()
	first.Scope.Levels[0].Value = id.NewAgentRunID().String()
	ctx := cortex.WithScope(context.Background(), first.Scope)
	requests := []execution.EffectRequest{first, first, first}
	for i := range requests {
		requests[i].OperationKey = id.New(execution.PrefixEffectAttempt).String()
		requests[i].AttemptID = id.New(execution.PrefixEffectAttempt)
		created, err := writes.BeginEffect(ctx, requests[i])
		require.NoError(t, err)
		require.True(t, created)
	}
	sort.Slice(requests, func(i, j int) bool {
		return evidence.Digest([]byte(requests[i].OperationKey)) < evidence.Digest([]byte(requests[j].OperationKey))
	})
	require.NoError(t, writes.CompleteEffect(ctx, requests[0], storedEffectReceipt(t, requests[0])))
	page, err := s.ListEffects(ctx, execution.EffectListRequest{RunID: first.RunID, Limit: 2})
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.Equal(t, requests[0], page[0].Request)
	require.NotNil(t, page[0].Receipt, "terminal records count toward the bound")
	require.Equal(t, requests[1], page[1].Request)
	require.Nil(t, page[1].Receipt)
	cursor := evidence.Digest([]byte(page[1].Request.OperationKey))
	page, err = s.ListEffects(ctx, execution.EffectListRequest{After: cursor, Limit: 2})
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, requests[2], page[0].Request)
	page[0].Request.Scope.Levels[0].Value = "mutated"
	page, err = s.ListEffects(ctx, execution.EffectListRequest{After: cursor, Limit: 2})
	require.NoError(t, err)
	require.Equal(t, requests[2], page[0].Request)
	page, err = s.ListEffects(ctx, execution.EffectListRequest{RunID: id.NewAgentRunID(), Limit: 2})
	require.NoError(t, err)
	require.Empty(t, page)
	for _, scope := range []cortex.Scope{
		{Levels: append([]cortex.Level(nil), first.Scope.Levels[:1]...)},
		{Levels: []cortex.Level{{Key: "org", Value: "other"}}},
	} {
		page, err = s.ListEffects(cortex.WithScope(context.Background(), scope), execution.EffectListRequest{Limit: 2})
		require.NoError(t, err)
		require.Empty(t, page)
	}
	_, err = s.ListEffects(context.Background(), execution.EffectListRequest{Limit: 2})
	require.ErrorIs(t, err, cortex.ErrNoScope)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.ListEffects(cancelled, execution.EffectListRequest{Limit: 2})
	require.ErrorIs(t, err, context.Canceled)
	for _, request := range []execution.EffectListRequest{{}, {Limit: 257}, {Limit: 1, After: "bad"}, {Limit: 1, RunID: id.NewAgentID()}} {
		_, err = s.ListEffects(ctx, request)
		require.ErrorIs(t, err, execution.ErrInvalidEffect)
	}
}
