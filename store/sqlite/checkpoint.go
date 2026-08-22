package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
)

func (s *Store) CreateCheckpoint(ctx context.Context, cp *checkpoint.Checkpoint) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	now := time.Now().UTC()
	cp.CreatedAt = now
	cp.UpdatedAt = now
	cp.Scope = scope
	m := checkpointToModel(cp)
	_, err := s.sdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: create checkpoint: %w", err)
	}
	return nil
}

func (s *Store) GetCheckpoint(ctx context.Context, cpID id.CheckpointID) (*checkpoint.Checkpoint, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(checkpointModel)
	q := s.sdb.NewSelect(m).Where("id = ?", cpID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	err := q.Scan(ctx)
	if err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrCheckpointNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get checkpoint: %w", err)
	}
	return checkpointFromModel(m)
}

func (s *Store) Resolve(ctx context.Context, cpID id.CheckpointID, decision checkpoint.Decision) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	decisionJSON, marshalErr := json.Marshal(decision)
	if marshalErr != nil {
		return fmt.Errorf("cortex/sqlite: marshal decision: %w", marshalErr)
	}
	q := s.sdb.NewUpdate((*checkpointModel)(nil)).
		Set("state = ?", "resolved").
		Set("decision = ?", string(decisionJSON)).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", cpID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: resolve checkpoint: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex/sqlite: resolve checkpoint rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrCheckpointNotFound
	}
	return nil
}

func (s *Store) ListPending(ctx context.Context, filter *checkpoint.ListFilter) ([]*checkpoint.Checkpoint, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var models []checkpointModel
	q := s.sdb.NewSelect(&models).
		Where("state = ?", "pending").
		OrderExpr("created_at ASC")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if filter != nil {
		if filter.RunID != "" {
			q = q.Where("run_id = ?", filter.RunID)
		}
		if filter.Limit > 0 {
			q = q.Limit(filter.Limit)
		}
		if filter.Offset > 0 {
			q = q.Offset(filter.Offset)
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/sqlite: list pending checkpoints: %w", err)
	}
	result := make([]*checkpoint.Checkpoint, len(models))
	for i := range models {
		cp, convErr := checkpointFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = cp
	}
	return result, nil
}

func (s *Store) CountPending(ctx context.Context, filter *checkpoint.ListFilter) (int64, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return 0, cortex.ErrNoScope
	}
	q := s.sdb.NewSelect((*checkpointModel)(nil)).
		Where("state = ?", "pending")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if filter != nil {
		if filter.RunID != "" {
			q = q.Where("run_id = ?", filter.RunID)
		}
	}
	count, err := q.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/sqlite: count pending checkpoints: %w", err)
	}
	return count, nil
}
