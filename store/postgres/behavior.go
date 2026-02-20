package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/id"
)

func (s *Store) CreateBehavior(ctx context.Context, b *behavior.Behavior) error {
	now := time.Now().UTC()
	b.CreatedAt = now
	b.UpdatedAt = now
	m := behaviorToModel(b)
	_, err := s.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: create behavior: %w", err)
	}
	return nil
}

func (s *Store) GetBehavior(ctx context.Context, behaviorID id.BehaviorID) (*behavior.Behavior, error) {
	m := new(behaviorModel)
	err := s.db.NewSelect().Model(m).Where("id = ?", behaviorID.String()).Scan(ctx)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, cortex.ErrBehaviorNotFound
		}
		return nil, fmt.Errorf("cortex: get behavior: %w", err)
	}
	return behaviorFromModel(m), nil
}

func (s *Store) GetBehaviorByName(ctx context.Context, appID, name string) (*behavior.Behavior, error) {
	m := new(behaviorModel)
	err := s.db.NewSelect().Model(m).
		Where("app_id = ?", appID).
		Where("name = ?", name).
		Scan(ctx)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, cortex.ErrBehaviorNotFound
		}
		return nil, fmt.Errorf("cortex: get behavior by name: %w", err)
	}
	return behaviorFromModel(m), nil
}

func (s *Store) UpdateBehavior(ctx context.Context, b *behavior.Behavior) error {
	b.UpdatedAt = time.Now().UTC()
	m := behaviorToModel(b)
	res, err := s.db.NewUpdate().Model(m).WherePK().Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: update behavior: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return cortex.ErrBehaviorNotFound
	}
	return nil
}

func (s *Store) DeleteBehavior(ctx context.Context, behaviorID id.BehaviorID) error {
	res, err := s.db.NewDelete().
		Model((*behaviorModel)(nil)).
		Where("id = ?", behaviorID.String()).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: delete behavior: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return cortex.ErrBehaviorNotFound
	}
	return nil
}

func (s *Store) ListBehaviors(ctx context.Context, filter *behavior.ListFilter) ([]*behavior.Behavior, error) {
	var models []behaviorModel
	q := s.db.NewSelect().Model(&models).OrderExpr("created_at ASC")
	if filter != nil {
		if filter.AppID != "" {
			q = q.Where("app_id = ?", filter.AppID)
		}
		if filter.Limit > 0 {
			q = q.Limit(filter.Limit)
		}
		if filter.Offset > 0 {
			q = q.Offset(filter.Offset)
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list behaviors: %w", err)
	}
	result := make([]*behavior.Behavior, len(models))
	for i := range models {
		result[i] = behaviorFromModel(&models[i])
	}
	return result, nil
}
