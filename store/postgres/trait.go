package postgres

import (
	"context"
	"database/sql"
	"errors"
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
	_, err := s.pgdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: create trait: %w", err)
	}
	return nil
}

func (s *Store) GetTrait(ctx context.Context, traitID id.TraitID) (*trait.Trait, error) {
	m := new(traitModel)
	err := s.pgdb.NewSelect(m).Where("id = ?", traitID.String()).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, cortex.ErrTraitNotFound
		}
		return nil, fmt.Errorf("cortex: get trait: %w", err)
	}
	return traitFromModel(m)
}

func (s *Store) GetTraitByName(ctx context.Context, appID, name string) (*trait.Trait, error) {
	m := new(traitModel)
	err := s.pgdb.NewSelect(m).
		Where("app_id = ?", appID).
		Where("name = ?", name).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, cortex.ErrTraitNotFound
		}
		return nil, fmt.Errorf("cortex: get trait by name: %w", err)
	}
	return traitFromModel(m)
}

func (s *Store) UpdateTrait(ctx context.Context, t *trait.Trait) error {
	t.UpdatedAt = time.Now().UTC()
	m := traitToModel(t)
	res, err := s.pgdb.NewUpdate(m).WherePK().Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: update trait: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cortex: update trait rows affected: %w", err)
	}
	if n == 0 {
		return cortex.ErrTraitNotFound
	}
	return nil
}

func (s *Store) DeleteTrait(ctx context.Context, traitID id.TraitID) error {
	res, err := s.pgdb.NewDelete((*traitModel)(nil)).
		Where("id = ?", traitID.String()).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: delete trait: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cortex: delete trait rows affected: %w", err)
	}
	if n == 0 {
		return cortex.ErrTraitNotFound
	}
	return nil
}

func (s *Store) ListTraits(ctx context.Context, filter *trait.ListFilter) ([]*trait.Trait, error) {
	var models []traitModel
	q := s.pgdb.NewSelect(&models).OrderExpr("created_at ASC")
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
		return nil, fmt.Errorf("cortex: list traits: %w", err)
	}
	result := make([]*trait.Trait, len(models))
	for i := range models {
		t, err := traitFromModel(&models[i])
		if err != nil {
			return nil, fmt.Errorf("cortex: list traits: %w", err)
		}
		result[i] = t
	}
	return result, nil
}
