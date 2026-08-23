package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/trait"
)

// CreateTrait persists a new trait, stamping the scope from the context.
// The (scope_canon, name) unique index is what actually enforces
// per-scope name uniqueness; app_id survives on the document as a
// vestigial field no longer read or filtered on.
func (s *Store) CreateTrait(ctx context.Context, t *trait.Trait) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	ts := now()
	t.CreatedAt = ts
	t.UpdatedAt = ts
	t.Scope = scope
	m := traitToModel(t)

	_, err := s.mdb.NewInsert(m).Exec(ctx)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/mongo: create trait: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex/mongo: create trait: %w", err)
	}

	return nil
}

// GetTrait returns a trait by ID within the caller's scope.
func (s *Store) GetTrait(ctx context.Context, traitID id.TraitID) (*trait.Trait, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var m traitModel

	filter := bson.M{"_id": traitID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	err := s.mdb.NewFind(&m).
		Filter(filter).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrTraitNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get trait: %w", err)
	}

	return traitFromModel(&m)
}

// GetTraitByName returns a trait by name within the caller's scope.
func (s *Store) GetTraitByName(ctx context.Context, name string) (*trait.Trait, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var m traitModel

	filter := bson.M{"name": name}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	err := s.mdb.NewFind(&m).
		Filter(filter).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrTraitNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get trait by name: %w", err)
	}

	return traitFromModel(&m)
}

// UpdateTrait modifies an existing trait's mutable fields within the
// caller's scope. Scope is immutable after creation: the context scope
// is used only as an authorization predicate (the caller must be at or
// above the trait's stored scope to touch it), and is never written
// back. grove's NewUpdate(model).Exec defaults to a full-field $set built
// from the model struct when no explicit update document is given, which
// would otherwise blank scope_l0/l1/l2/extra/canon on every call.
//
// app_id is deliberately absent from set too, for the same reason:
// traitToModel always writes AppID as "" now (Trait carries no such
// field to draw from), so including it here would blank whatever a
// pre-v1.8.0 document's app_id field still holds on its very first
// Update. The field itself is vestigial but intentionally left in place;
// erasing its content is not this task's call to make.
func (s *Store) UpdateTrait(ctx context.Context, t *trait.Trait) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	t.UpdatedAt = now()
	m := traitToModel(t)

	filter := bson.M{"_id": m.ID}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	set := bson.M{
		"name":        m.Name,
		"description": m.Description,
		"dimensions":  m.Dimensions,
		"influences":  m.Influences,
		"category":    m.Category,
		"metadata":    m.Metadata,
		"updated_at":  m.UpdatedAt,
	}

	res, err := s.mdb.NewUpdate((*traitModel)(nil)).
		Filter(filter).
		SetUpdate(bson.M{"$set": set}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update trait: %w", err)
	}

	if res.MatchedCount() == 0 {
		return cortex.ErrTraitNotFound
	}

	return nil
}

// DeleteTrait removes a trait within the caller's scope.
func (s *Store) DeleteTrait(ctx context.Context, traitID id.TraitID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	filter := bson.M{"_id": traitID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	res, err := s.mdb.NewDelete((*traitModel)(nil)).
		Filter(filter).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: delete trait: %w", err)
	}

	if res.DeletedCount() == 0 {
		return cortex.ErrTraitNotFound
	}

	return nil
}

// ListTraits returns traits within the caller's scope, optionally filtered.
func (s *Store) ListTraits(ctx context.Context, filter *trait.ListFilter) ([]*trait.Trait, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &trait.ListFilter{}
	}
	var models []traitModel

	f := bson.M{}
	for k, v := range scopeFilter(scope, filter.Exact) {
		f[k] = v
	}
	if filter.Category != "" {
		f["category"] = string(filter.Category)
	}
	if filter.Search != "" {
		f["name"] = bson.M{"$regex": filter.Search, "$options": "i"}
	}

	q := s.mdb.NewFind(&models).
		Filter(f).
		Sort(bson.D{{Key: "created_at", Value: 1}})

	if filter.Limit > 0 {
		q = q.Limit(int64(filter.Limit))
	}

	if filter.Offset > 0 {
		q = q.Skip(int64(filter.Offset))
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

// CountTraits returns the total number of traits matching the filter
// within the caller's scope.
func (s *Store) CountTraits(ctx context.Context, filter *trait.ListFilter) (int64, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return 0, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &trait.ListFilter{}
	}
	f := bson.M{}
	for k, v := range scopeFilter(scope, filter.Exact) {
		f[k] = v
	}
	if filter.Category != "" {
		f["category"] = string(filter.Category)
	}
	if filter.Search != "" {
		f["name"] = bson.M{"$regex": filter.Search, "$options": "i"}
	}

	count, err := s.mdb.NewFind((*traitModel)(nil)).
		Filter(f).
		Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/mongo: count traits: %w", err)
	}

	return count, nil
}
