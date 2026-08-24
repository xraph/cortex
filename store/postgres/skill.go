package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/skill"
)

// CreateSkill persists a new skill, stamping the scope from the context.
// UNIQUE (scope_canon, name) is what actually enforces per-scope name
// uniqueness; app_id survives on the row as a vestigial column no longer
// read or filtered on.
func (s *Store) CreateSkill(ctx context.Context, sk *skill.Skill) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	now := time.Now().UTC()
	sk.CreatedAt = now
	sk.UpdatedAt = now
	sk.Scope = scope
	m := skillToModel(sk)
	_, err := s.pgdb.NewInsert(m).Exec(ctx)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex: create skill: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex: create skill: %w", err)
	}
	return nil
}

func (s *Store) GetSkill(ctx context.Context, skillID id.SkillID) (*skill.Skill, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(skillModel)
	q := s.pgdb.NewSelect(m).Where("id = ?", skillID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	err := q.Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, cortex.ErrSkillNotFound
		}
		return nil, fmt.Errorf("cortex: get skill: %w", err)
	}
	return skillFromModel(m)
}

func (s *Store) GetSkillByName(ctx context.Context, name string) (*skill.Skill, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(skillModel)
	q := s.pgdb.NewSelect(m).Where("name = ?", name)
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	err := q.Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, cortex.ErrSkillNotFound
		}
		return nil, fmt.Errorf("cortex: get skill by name: %w", err)
	}
	return skillFromModel(m)
}

// mutableSkillColumns is every cortex_skills column UpdateSkill is
// allowed to write. A skill's scope is set once at creation and never
// rewritten: scope_l0/l1/l2/extra/canon are deliberately absent here.
// Grove's NewUpdate builds SET from every model field by default, so
// without this explicit whitelist UpdateSkill would blank the scope
// columns on every call (skillToModel always derives them from
// sk.Scope, and nothing populates sk.Scope on the update path).
//
// app_id is deliberately absent too, for the same reason: skillToModel
// always writes AppID as "" now (Skill carries no such field to draw
// from), so including "app_id" here would blank whatever a pre-v1.8.0
// row's app_id column still holds on its very first update. The column
// itself is vestigial but intentionally left in place; erasing its
// content belongs to whatever change finally drops the column.
var mutableSkillColumns = []string{
	"name",
	"description",
	"tools",
	"knowledge",
	"system_prompt_fragment",
	"dependencies",
	"default_proficiency",
	"metadata",
	"created_at",
	"updated_at",
}

// UpdateSkill modifies an existing skill's mutable fields within the
// caller's scope. Scope is immutable after creation: the context scope is
// used only as an authorization predicate (the caller must be at or above
// the skill's stored scope to touch it), and is never written back —
// mutableSkillColumns excludes the five scope columns from what gets
// written.
func (s *Store) UpdateSkill(ctx context.Context, sk *skill.Skill) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	sk.UpdatedAt = time.Now().UTC()
	m := skillToModel(sk)
	q := s.pgdb.NewUpdate(m).Column(mutableSkillColumns...).WherePK()
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: update skill: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cortex: update skill rows affected: %w", err)
	}
	if n == 0 {
		return cortex.ErrSkillNotFound
	}
	return nil
}

func (s *Store) DeleteSkill(ctx context.Context, skillID id.SkillID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	q := s.pgdb.NewDelete((*skillModel)(nil)).
		Where("id = ?", skillID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: delete skill: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cortex: delete skill rows affected: %w", err)
	}
	if n == 0 {
		return cortex.ErrSkillNotFound
	}
	return nil
}

func (s *Store) ListSkills(ctx context.Context, filter *skill.ListFilter) ([]*skill.Skill, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &skill.ListFilter{}
	}
	var models []skillModel
	q := s.pgdb.NewSelect(&models).OrderExpr("created_at ASC")
	for _, p := range scopePredicates(scope, filter.Exact) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if filter.Search != "" {
		q = q.Where("LOWER(name) LIKE LOWER(?)", "%"+filter.Search+"%")
	}
	if filter.Limit > 0 {
		q = q.Limit(filter.Limit)
	}
	if filter.Offset > 0 {
		q = q.Offset(filter.Offset)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list skills: %w", err)
	}
	result := make([]*skill.Skill, len(models))
	for i := range models {
		sk, err := skillFromModel(&models[i])
		if err != nil {
			return nil, fmt.Errorf("cortex: list skills: %w", err)
		}
		result[i] = sk
	}
	return result, nil
}

func (s *Store) CountSkills(ctx context.Context, filter *skill.ListFilter) (int64, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return 0, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &skill.ListFilter{}
	}
	q := s.pgdb.NewSelect((*skillModel)(nil))
	for _, p := range scopePredicates(scope, filter.Exact) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if filter.Search != "" {
		q = q.Where("LOWER(name) LIKE LOWER(?)", "%"+filter.Search+"%")
	}
	count, err := q.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex: count skills: %w", err)
	}
	return count, nil
}
