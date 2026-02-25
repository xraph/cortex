package sqlite

import (
	"context"
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
	_, err := s.sdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: create behavior: %w", err)
	}
	return nil
}

func (s *Store) GetBehavior(ctx context.Context, behaviorID id.BehaviorID) (*behavior.Behavior, error) {
	m := new(behaviorModel)
	err := s.sdb.NewSelect(m).Where("id = ?", behaviorID.String()).Scan(ctx)
	if err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrBehaviorNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get behavior: %w", err)
	}
	return behaviorFromModel(m)
}

func (s *Store) GetBehaviorByName(ctx context.Context, appID, name string) (*behavior.Behavior, error) {
	m := new(behaviorModel)
	err := s.sdb.NewSelect(m).
		Where("app_id = ?", appID).
		Where("name = ?", name).
		Scan(ctx)
	if err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrBehaviorNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get behavior by name: %w", err)
	}
	return behaviorFromModel(m)
}

func (s *Store) UpdateBehavior(ctx context.Context, b *behavior.Behavior) error {
	b.UpdatedAt = time.Now().UTC()
	m := behaviorToModel(b)
	res, err := s.sdb.NewUpdate(m).WherePK().Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: update behavior: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex/sqlite: update behavior rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrBehaviorNotFound
	}
	return nil
}

func (s *Store) DeleteBehavior(ctx context.Context, behaviorID id.BehaviorID) error {
	res, err := s.sdb.NewDelete((*behaviorModel)(nil)).
		Where("id = ?", behaviorID.String()).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: delete behavior: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex/sqlite: delete behavior rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrBehaviorNotFound
	}
	return nil
}

func (s *Store) ListBehaviors(ctx context.Context, filter *behavior.ListFilter) ([]*behavior.Behavior, error) {
	var models []behaviorModel
	q := s.sdb.NewSelect(&models).OrderExpr("created_at ASC")
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
		return nil, fmt.Errorf("cortex/sqlite: list behaviors: %w", err)
	}
	result := make([]*behavior.Behavior, len(models))
	for i := range models {
		b, convErr := behaviorFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = b
	}
	return result, nil
}
