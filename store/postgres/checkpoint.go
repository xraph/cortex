package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
)

func (s *Store) CreateCheckpoint(ctx context.Context, cp *checkpoint.Checkpoint) error {
	now := time.Now().UTC()
	cp.CreatedAt = now
	cp.UpdatedAt = now
	m := checkpointToModel(cp)
	_, err := s.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: create checkpoint: %w", err)
	}
	return nil
}

func (s *Store) GetCheckpoint(ctx context.Context, cpID id.CheckpointID) (*checkpoint.Checkpoint, error) {
	m := new(checkpointModel)
	err := s.db.NewSelect().Model(m).Where("id = ?", cpID.String()).Scan(ctx)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, cortex.ErrCheckpointNotFound
		}
		return nil, fmt.Errorf("cortex: get checkpoint: %w", err)
	}
	return checkpointFromModel(m), nil
}

func (s *Store) Resolve(ctx context.Context, cpID id.CheckpointID, decision checkpoint.Decision) error {
	decisionJSON, _ := json.Marshal(decision)
	res, err := s.db.NewUpdate().
		Model((*checkpointModel)(nil)).
		Set("state = ?", "resolved").
		Set("decision = ?", string(decisionJSON)).
		Set("updated_at = ?", time.Now().UTC()).
		Where("id = ?", cpID.String()).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: resolve checkpoint: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return cortex.ErrCheckpointNotFound
	}
	return nil
}

func (s *Store) ListPending(ctx context.Context, filter *checkpoint.ListFilter) ([]*checkpoint.Checkpoint, error) {
	var models []checkpointModel
	q := s.db.NewSelect().Model(&models).
		Where("state = ?", "pending").
		OrderExpr("created_at ASC")
	if filter != nil {
		if filter.RunID != "" {
			q = q.Where("run_id = ?", filter.RunID)
		}
		if filter.TenantID != "" {
			q = q.Where("tenant_id = ?", filter.TenantID)
		}
		if filter.Limit > 0 {
			q = q.Limit(filter.Limit)
		}
		if filter.Offset > 0 {
			q = q.Offset(filter.Offset)
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list pending checkpoints: %w", err)
	}
	result := make([]*checkpoint.Checkpoint, len(models))
	for i := range models {
		result[i] = checkpointFromModel(&models[i])
	}
	return result, nil
}
