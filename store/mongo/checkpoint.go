package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
)

// CreateCheckpoint persists a new checkpoint.
func (s *Store) CreateCheckpoint(ctx context.Context, cp *checkpoint.Checkpoint) error {
	t := now()
	cp.CreatedAt = t
	cp.UpdatedAt = t
	m := checkpointToModel(cp)

	_, err := s.mdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: create checkpoint: %w", err)
	}

	return nil
}

// GetCheckpoint returns a checkpoint by ID.
func (s *Store) GetCheckpoint(ctx context.Context, cpID id.CheckpointID) (*checkpoint.Checkpoint, error) {
	var m checkpointModel

	err := s.mdb.NewFind(&m).
		Filter(bson.M{"_id": cpID.String()}).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrCheckpointNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get checkpoint: %w", err)
	}

	return checkpointFromModel(&m)
}

// Resolve resolves a pending checkpoint with a decision.
func (s *Store) Resolve(ctx context.Context, cpID id.CheckpointID, decision checkpoint.Decision) error {
	t := now()

	res, err := s.mdb.NewUpdate((*checkpointModel)(nil)).
		Filter(bson.M{"_id": cpID.String()}).
		Set("state", "resolved").
		Set("decision", decision).
		Set("updated_at", t).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: resolve checkpoint: %w", err)
	}

	if res.MatchedCount() == 0 {
		return cortex.ErrCheckpointNotFound
	}

	return nil
}

// ListPending returns pending checkpoints, optionally filtered.
func (s *Store) ListPending(ctx context.Context, filter *checkpoint.ListFilter) ([]*checkpoint.Checkpoint, error) {
	var models []checkpointModel

	f := bson.M{"state": "pending"}
	if filter != nil {
		if filter.RunID != "" {
			f["run_id"] = filter.RunID
		}

		if filter.TenantID != "" {
			f["tenant_id"] = filter.TenantID
		}
	}

	q := s.mdb.NewFind(&models).
		Filter(f).
		Sort(bson.D{{Key: "created_at", Value: 1}})

	if filter != nil {
		if filter.Limit > 0 {
			q = q.Limit(int64(filter.Limit))
		}

		if filter.Offset > 0 {
			q = q.Skip(int64(filter.Offset))
		}
	}

	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/mongo: list pending checkpoints: %w", err)
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
