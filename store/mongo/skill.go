package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/skill"
)

// CreateSkill persists a new skill, stamping the scope from the context.
// The (scope_canon, name) unique index is what actually enforces
// per-scope name uniqueness; app_id survives on the document as a
// vestigial field no longer read or filtered on.
func (s *Store) CreateSkill(ctx context.Context, sk *skill.Skill) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	t := now()
	sk.CreatedAt = t
	sk.UpdatedAt = t
	sk.Scope = scope
	m := skillToModel(sk)

	_, err := s.mdb.NewInsert(m).Exec(ctx)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/mongo: create skill: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex/mongo: create skill: %w", err)
	}

	return nil
}

// GetSkill returns a skill by ID within the caller's scope.
func (s *Store) GetSkill(ctx context.Context, skillID id.SkillID) (*skill.Skill, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var m skillModel

	filter := bson.M{"_id": skillID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	err := s.mdb.NewFind(&m).
		Filter(filter).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrSkillNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get skill: %w", err)
	}

	return skillFromModel(&m)
}

// GetSkillByName returns a skill by name within the caller's scope.
func (s *Store) GetSkillByName(ctx context.Context, name string) (*skill.Skill, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var m skillModel

	filter := bson.M{"name": name}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	err := s.mdb.NewFind(&m).
		Filter(filter).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrSkillNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get skill by name: %w", err)
	}

	return skillFromModel(&m)
}

// UpdateSkill modifies an existing skill's mutable fields within the
// caller's scope. Scope is immutable after creation: the context scope
// is used only as an authorization predicate (the caller must be at or
// above the skill's stored scope to touch it), and is never written
// back. grove's NewUpdate(model).Exec defaults to a full-field $set built
// from the model struct when no explicit update document is given, which
// would otherwise blank scope_l0/l1/l2/extra/canon on every call.
//
// app_id is deliberately absent from set too, for the same reason:
// skillToModel always writes AppID as "" now (Skill carries no such
// field to draw from), so including it here would blank whatever a
// pre-v1.8.0 document's app_id field still holds on its very first
// Update. The field itself is vestigial but intentionally left in place;
// erasing its content is not this task's call to make.
func (s *Store) UpdateSkill(ctx context.Context, sk *skill.Skill) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	sk.UpdatedAt = now()
	m := skillToModel(sk)

	filter := bson.M{"_id": m.ID}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	set := bson.M{
		"name":                   m.Name,
		"description":            m.Description,
		"tools":                  m.Tools,
		"knowledge":              m.Knowledge,
		"system_prompt_fragment": m.SystemPromptFragment,
		"dependencies":           m.Dependencies,
		"default_proficiency":    m.DefaultProficiency,
		"metadata":               m.Metadata,
		"updated_at":             m.UpdatedAt,
	}

	res, err := s.mdb.NewUpdate((*skillModel)(nil)).
		Filter(filter).
		SetUpdate(bson.M{"$set": set}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update skill: %w", err)
	}

	if res.MatchedCount() == 0 {
		return cortex.ErrSkillNotFound
	}

	return nil
}

// DeleteSkill removes a skill within the caller's scope.
func (s *Store) DeleteSkill(ctx context.Context, skillID id.SkillID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	filter := bson.M{"_id": skillID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	res, err := s.mdb.NewDelete((*skillModel)(nil)).
		Filter(filter).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: delete skill: %w", err)
	}

	if res.DeletedCount() == 0 {
		return cortex.ErrSkillNotFound
	}

	return nil
}

// ListSkills returns skills within the caller's scope, optionally filtered.
func (s *Store) ListSkills(ctx context.Context, filter *skill.ListFilter) ([]*skill.Skill, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &skill.ListFilter{}
	}
	var models []skillModel

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
		return nil, fmt.Errorf("cortex/mongo: list skills: %w", err)
	}

	result := make([]*skill.Skill, len(models))
	for i := range models {
		sk, convErr := skillFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = sk
	}

	return result, nil
}

// CountSkills returns the total number of skills matching the filter
// within the caller's scope.
func (s *Store) CountSkills(ctx context.Context, filter *skill.ListFilter) (int64, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return 0, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &skill.ListFilter{}
	}
	f := bson.M{}
	for k, v := range scopeFilter(scope, filter.Exact) {
		f[k] = v
	}
	if filter.Search != "" {
		f["name"] = bson.M{"$regex": filter.Search, "$options": "i"}
	}

	count, err := s.mdb.NewFind((*skillModel)(nil)).
		Filter(f).
		Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/mongo: count skills: %w", err)
	}

	return count, nil
}
