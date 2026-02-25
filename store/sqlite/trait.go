package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/trait"
)

func (s *Store) CreateTrait(ctx context.Context, t *trait.Trait) error {
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	m := traitToModel(t)
	_, err := s.sdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: create trait: %w", err)
	}
	return nil
}

func (s *Store) GetTrait(ctx context.Context, traitID id.TraitID) (*trait.Trait, error) {
	m := new(traitModel)
	err := s.sdb.NewSelect(m).Where("id = ?", traitID.String()).Scan(ctx)
	if err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrTraitNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get trait: %w", err)
	}
	return traitFromModel(m)
}

func (s *Store) GetTraitByName(ctx context.Context, appID, name string) (*trait.Trait, error) {
	m := new(traitModel)
	err := s.sdb.NewSelect(m).
		Where("app_id = ?", appID).
		Where("name = ?", name).
		Scan(ctx)
	if err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrTraitNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get trait by name: %w", err)
	}
	return traitFromModel(m)
}

func (s *Store) UpdateTrait(ctx context.Context, t *trait.Trait) error {
	t.UpdatedAt = time.Now().UTC()
	m := traitToModel(t)
	res, err := s.sdb.NewUpdate(m).WherePK().Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: update trait: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex/sqlite: update trait rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrTraitNotFound
	}
	return nil
}

func (s *Store) DeleteTrait(ctx context.Context, traitID id.TraitID) error {
	res, err := s.sdb.NewDelete((*traitModel)(nil)).
		Where("id = ?", traitID.String()).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: delete trait: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex/sqlite: delete trait rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrTraitNotFound
	}
	return nil
}

func (s *Store) ListTraits(ctx context.Context, filter *trait.ListFilter) ([]*trait.Trait, error) {
	var models []traitModel
	q := s.sdb.NewSelect(&models).OrderExpr("created_at ASC")
	if filter != nil {
		if filter.AppID != "" {
			q = q.Where("app_id = ?", filter.AppID)
		}
		if filter.Category != "" {
			q = q.Where("category = ?", string(filter.Category))
		}
		if filter.Limit > 0 {
			q = q.Limit(filter.Limit)
		}
		if filter.Offset > 0 {
			q = q.Offset(filter.Offset)
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/sqlite: list traits: %w", err)
	}
	result := make([]*trait.Trait, len(models))
	for i := range models {
		t, convErr := traitFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = t
	}
	return result, nil
}
