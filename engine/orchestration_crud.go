package engine

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

// CreateOrchestration stores a new orchestration config.
func (e *Engine) CreateOrchestration(ctx context.Context, c *orchestration.OrchestrationConfig) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.CreateOrchestration(ctx, c)
}

// GetOrchestration returns an orchestration config by ID.
func (e *Engine) GetOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) (*orchestration.OrchestrationConfig, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetOrchestration(ctx, orchID)
}

// GetOrchestrationByName returns an orchestration config by app-scoped name.
func (e *Engine) GetOrchestrationByName(ctx context.Context, appID, name string) (*orchestration.OrchestrationConfig, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetOrchestrationByName(ctx, appID, name)
}

// UpdateOrchestration updates an existing orchestration config.
func (e *Engine) UpdateOrchestration(ctx context.Context, c *orchestration.OrchestrationConfig) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.UpdateOrchestration(ctx, c)
}

// DeleteOrchestration removes an orchestration config.
func (e *Engine) DeleteOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.DeleteOrchestration(ctx, orchID)
}

// ListOrchestrations lists orchestration configs.
func (e *Engine) ListOrchestrations(ctx context.Context, filter *orchestration.ConfigListFilter) ([]*orchestration.OrchestrationConfig, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListOrchestrations(ctx, filter)
}

// CountOrchestrations counts orchestration configs.
func (e *Engine) CountOrchestrations(ctx context.Context, filter *orchestration.ConfigListFilter) (int64, error) {
	if e.store == nil {
		return 0, cortex.ErrNoStore
	}
	return e.store.CountOrchestrations(ctx, filter)
}

// GetOrchestrationRun returns an orchestration run record by ID.
func (e *Engine) GetOrchestrationRun(ctx context.Context, runID id.OrchestrationID) (*orchestration.OrchestrationRun, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetOrchestrationRun(ctx, runID)
}

// ListOrchestrationRuns lists orchestration run records.
func (e *Engine) ListOrchestrationRuns(ctx context.Context, filter *orchestration.RunListFilter) ([]*orchestration.OrchestrationRun, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListOrchestrationRuns(ctx, filter)
}

// CountOrchestrationRuns counts orchestration run records.
func (e *Engine) CountOrchestrationRuns(ctx context.Context, filter *orchestration.RunListFilter) (int64, error) {
	if e.store == nil {
		return 0, cortex.ErrNoStore
	}
	return e.store.CountOrchestrationRuns(ctx, filter)
}
