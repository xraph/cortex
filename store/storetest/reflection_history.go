package storetest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/store"
)

func reflectionHistory(t *testing.T, backend store.Store) {
	ctx, req := reflectionFixture(t, backend)
	writes := resultCapability[cognitive.ReflectionStore](t, backend)
	reads := resultCapability[cognitive.ReflectionHistoryStore](t, backend)
	for range 3 {
		receipt, err := writes.AdmitReflection(ctx, req)
		require.NoError(t, err)
		req.ExpectedVersion = receipt.State.Version
		req.ExpectedControlRevision = receipt.State.ControlRevision
	}
	page, err := reads.ListReflections(ctx, req.Snapshot.Binding.RunID, 0, 2)
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	require.Equal(t, int64(1), page.Items[0].State.Version)
	require.Equal(t, int64(2), page.After)
	page, err = reads.ListReflections(ctx, req.Snapshot.Binding.RunID, page.After, 2)
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, int64(3), page.Items[0].State.Version)
	require.Zero(t, page.After)
	require.False(t, page.Items[0].Fresh)
	other := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "other"}}})
	page, err = reads.ListReflections(other, req.Snapshot.Binding.RunID, 0, 2)
	require.NoError(t, err)
	require.Empty(t, page.Items)
	_, err = reads.ListReflections(context.Background(), req.Snapshot.Binding.RunID, 0, 2)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	_, err = reads.ListReflections(ctx, req.Snapshot.Binding.RunID, -1, 2)
	require.Error(t, err)
}
