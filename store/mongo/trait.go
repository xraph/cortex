package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/trait"
)

// CreateTrait persists a new trait.
func (s *Store) CreateTrait(ctx context.Context, t *trait.Trait) error {
	ts := now()
	t.CreatedAt = ts
	t.UpdatedAt = ts
	m := traitToModel(t)

	_, err := s.mdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: create trait: %w", err)
	}

	return nil
}

// GetTrait returns a trait by ID.
func (s *Store) GetTrait(ctx context.Context, traitID id.TraitID) (*trait.Trait, error) {
	var m traitModel

	err := s.mdb.NewFind(&m).
		Filter(bson.M{"_id": traitID.String()}).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrTraitNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get trait: %w", err)
	}

	return traitFromModel(&m)
}

// GetTraitByName returns a trait by app ID and name.
func (s *Store) GetTraitByName(ctx context.Context, appID, name string) (*trait.Trait, error) {
	var m traitModel

	err := s.mdb.NewFind(&m).
		Filter(bson.M{"app_id": appID, "name": name}).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrTraitNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get trait by name: %w", err)
	}

	return traitFromModel(&m)
}

// UpdateTrait modifies an existing trait.
func (s *Store) UpdateTrait(ctx context.Context, t *trait.Trait) error {
	t.UpdatedAt = now()
	m := traitToModel(t)

	res, err := s.mdb.NewUpdate(m).
		Filter(bson.M{"_id": m.ID}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update trait: %w", err)
	}

	if res.MatchedCount() == 0 {
		return cortex.ErrTraitNotFound
	}

	return nil
}

// DeleteTrait removes a trait.
func (s *Store) DeleteTrait(ctx context.Context, traitID id.TraitID) error {
	res, err := s.mdb.NewDelete((*traitModel)(nil)).
		Filter(bson.M{"_id": traitID.String()}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: delete trait: %w", err)
	}

	if res.DeletedCount() == 0 {
		return cortex.ErrTraitNotFound
	}

	return nil
}

// ListTraits returns traits, optionally filtered.
func (s *Store) ListTraits(ctx context.Context, filter *trait.ListFilter) ([]*trait.Trait, error) {
	var models []traitModel

	f := bson.M{}
	if filter != nil {
		if filter.AppID != "" {
			f["app_id"] = filter.AppID
		}

		if filter.Category != "" {
			f["category"] = string(filter.Category)
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
		return nil, fmt.Errorf("cortex/mongo: list traits: %w", err)
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
