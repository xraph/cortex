package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/id"
)

// CreateBehavior persists a new behavior, stamping the scope from the
// context. The (scope_canon, name) unique index is what actually
// enforces per-scope name uniqueness; app_id survives on the document as
// a vestigial field no longer read or filtered on.
func (s *Store) CreateBehavior(ctx context.Context, b *behavior.Behavior) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	t := now()
	b.CreatedAt = t
	b.UpdatedAt = t
	b.Scope = scope
	m := behaviorToModel(b)

	_, err := s.mdb.NewInsert(m).Exec(ctx)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/mongo: create behavior: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex/mongo: create behavior: %w", err)
	}

	return nil
}

// GetBehavior returns a behavior by ID within the caller's scope.
func (s *Store) GetBehavior(ctx context.Context, behaviorID id.BehaviorID) (*behavior.Behavior, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var m behaviorModel

	filter := bson.M{"_id": behaviorID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	err := s.mdb.NewFind(&m).
		Filter(filter).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrBehaviorNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get behavior: %w", err)
	}

	return behaviorFromModel(&m)
}

// GetBehaviorByName returns a behavior by name within the caller's scope.
func (s *Store) GetBehaviorByName(ctx context.Context, name string) (*behavior.Behavior, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var m behaviorModel

	filter := bson.M{"name": name}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	err := s.mdb.NewFind(&m).
		Filter(filter).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrBehaviorNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get behavior by name: %w", err)
	}

	return behaviorFromModel(&m)
}

// UpdateBehavior modifies an existing behavior's mutable fields within
// the caller's scope. Scope is immutable after creation: the context
// scope is used only as an authorization predicate (the caller must be
// at or above the behavior's stored scope to touch it), and is never
// written back. grove's NewUpdate(model).Exec defaults to a full-field
// $set built from the model struct when no explicit update document is
// given, which would otherwise blank scope_l0/l1/l2/extra/canon on every
// call.
//
// app_id is deliberately absent from set too, for the same reason:
// behaviorToModel always writes AppID as "" now (Behavior carries no
// such field to draw from), so including it here would blank whatever a
// pre-v1.8.0 document's app_id field still holds on its very first
// Update. The field itself is vestigial but intentionally left in place;
// erasing its content is not this task's call to make.
func (s *Store) UpdateBehavior(ctx context.Context, b *behavior.Behavior) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	b.UpdatedAt = now()
	m := behaviorToModel(b)

	filter := bson.M{"_id": m.ID}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	set := bson.M{
		"name":           m.Name,
		"description":    m.Description,
		"triggers":       m.Triggers,
		"actions":        m.Actions,
		"priority":       m.Priority,
		"requires_skill": m.RequiresSkill,
		"requires_trait": m.RequiresTrait,
		"metadata":       m.Metadata,
		"updated_at":     m.UpdatedAt,
	}

	res, err := s.mdb.NewUpdate((*behaviorModel)(nil)).
		Filter(filter).
		SetUpdate(bson.M{"$set": set}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update behavior: %w", err)
	}

	if res.MatchedCount() == 0 {
		return cortex.ErrBehaviorNotFound
	}

	return nil
}

// DeleteBehavior removes a behavior within the caller's scope.
func (s *Store) DeleteBehavior(ctx context.Context, behaviorID id.BehaviorID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	filter := bson.M{"_id": behaviorID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	res, err := s.mdb.NewDelete((*behaviorModel)(nil)).
		Filter(filter).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: delete behavior: %w", err)
	}

	if res.DeletedCount() == 0 {
		return cortex.ErrBehaviorNotFound
	}

	return nil
}

// ListBehaviors returns behaviors within the caller's scope, optionally filtered.
func (s *Store) ListBehaviors(ctx context.Context, filter *behavior.ListFilter) ([]*behavior.Behavior, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &behavior.ListFilter{}
	}
	var models []behaviorModel

	f := bson.M{}
	for k, v := range scopeFilter(scope, filter.Exact) {
		f[k] = v
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
		return nil, fmt.Errorf("cortex/mongo: list behaviors: %w", err)
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

// CountBehaviors returns the total number of behaviors matching the
// filter within the caller's scope.
func (s *Store) CountBehaviors(ctx context.Context, filter *behavior.ListFilter) (int64, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return 0, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &behavior.ListFilter{}
	}
	f := bson.M{}
	for k, v := range scopeFilter(scope, filter.Exact) {
		f[k] = v
	}
	if filter.Search != "" {
		f["name"] = bson.M{"$regex": filter.Search, "$options": "i"}
	}

	count, err := s.mdb.NewFind((*behaviorModel)(nil)).
		Filter(f).
		Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/mongo: count behaviors: %w", err)
	}

	return count, nil
}
