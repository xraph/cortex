package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/store/internal/orchestrationstate"
)

// LinkOrchestrationChild atomically admits one child while its parent is running.
func (s *Store) LinkOrchestrationChild(ctx context.Context, team id.OrchestrationID, child id.AgentRunID) error {
	if handled, err := s.UnownedOrchestrationParent(ctx, team, false, func(driver orchestrationstate.Driver) error { return orchestrationstate.Link(ctx, driver, team, child) }); handled {
		return err
	}
	return orchestrationstate.Link(ctx, orchestrationRunDriver{store: s}, team, child)
}

type orchestrationRunDriver struct {
	store        *Store
	queryBackend assuranceQueries
}

func (d orchestrationRunDriver) queries() assuranceQueries {
	if d.queryBackend != nil {
		return d.queryBackend
	}
	return d.store.sdb
}

// CoordinatorParentDriver binds parent CAS to the active budget transaction.
func (d assuranceDriver) CoordinatorParentDriver(ctx context.Context) (orchestrationstate.Driver, error) {
	if d.transaction == nil {
		return nil, cortex.ErrOrchestrationContinuationUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return orchestrationRunDriver{store: d.store, queryBackend: d.queries()}, nil
}

func (d orchestrationRunDriver) Read(ctx context.Context, key id.OrchestrationID) (orchestrationstate.Snapshot, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return orchestrationstate.Snapshot{}, err
	}
	model := new(orchestrationRunModel)
	query := d.queries().NewSelect(model).Where("id = ?", key.String())
	for _, p := range scopePredicates(scope, true) {
		query = query.Where(p.Column+" = ?", p.Value)
	}
	if err = query.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return orchestrationstate.Snapshot{}, cortex.ErrOrchestrationRunNotFound
		}
		return orchestrationstate.Snapshot{}, fmt.Errorf("cortex: read orchestration run revision: %w", err)
	}
	record, err := orchestrationRunFromModel(model)
	return orchestrationstate.Snapshot{Run: record, Revision: model.Revision}, err
}

func (d orchestrationRunDriver) CompareAndSwap(ctx context.Context, before orchestrationstate.Snapshot, after *orchestration.Run) (bool, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return false, err
	}
	model := orchestrationRunToModel(after)
	model.Revision = before.Revision + 1
	query := d.queries().NewUpdate(model).Column(mutableOrchestrationRunColumns...).Where("id = ?", before.Run.ID.String()).Where("revision = ?", before.Revision)
	for _, p := range scopePredicates(scope, true) {
		query = query.Where(p.Column+" = ?", p.Value)
	}
	result, err := query.Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("cortex: update orchestration run revision: %w", err)
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// ClaimOrchestrationContinuation elects one waiting team pass at the exact scope.
func (s *Store) ClaimOrchestrationContinuation(ctx context.Context, team id.OrchestrationID) (*orchestration.Run, error) {
	var result *orchestration.Run
	handled, err := s.UnownedOrchestrationParent(ctx, team, false, func(driver orchestrationstate.Driver) error {
		var claimErr error
		result, claimErr = orchestrationstate.ClaimContinuation(ctx, driver, team)
		return claimErr
	})
	if handled {
		if err != nil {
			return nil, err
		}
		return result, nil
	}
	return orchestrationstate.ClaimContinuation(ctx, orchestrationRunDriver{store: s}, team)
}
