package engine

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
)

// capturePreparation checks the provider contract even when a provider ignores
// collection errors. Both preparation and later source checks use this boundary.
func capturePreparation(ctx context.Context, provider store.PreparationSnapshotter, collect func(store.PreparationSources) error) (*run.SourceSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	calls := 0
	var collectionErr error
	receipt, err := provider.CapturePreparation(ctx, func(sources store.PreparationSources) error {
		calls++
		if calls != 1 || sources == nil {
			collectionErr = cortex.ErrPreparationUnavailable
			return collectionErr
		}
		collectionErr = collect(sources)
		return collectionErr
	})
	if err != nil {
		return nil, err
	}
	if collectionErr != nil {
		return nil, collectionErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if calls != 1 || receipt == nil || receipt.ID == "" || receipt.Provider == "" || receipt.Isolation == "" || receipt.Scope != cortex.ScopeFromContext(ctx).Canonical() {
		return nil, cortex.ErrPreparationUnavailable
	}
	detached := *receipt
	return &detached, nil
}
