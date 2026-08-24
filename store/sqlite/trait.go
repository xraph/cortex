package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/trait"
)

// CreateTrait persists a new trait, stamping the scope from the context.
// UNIQUE (scope_canon, name) is what actually enforces per-scope name
// uniqueness; app_id survives on the row as a vestigial column no longer
// read or filtered on.
func (s *Store) CreateTrait(ctx context.Context, t *trait.Trait) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	now := time.Now().UTC()
	t.CreatedAt = now
	t.UpdatedAt = now
	t.Scope = scope
	m := traitToModel(t)
	_, err := s.sdb.NewInsert(m).Exec(ctx)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/sqlite: create trait: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex/sqlite: create trait: %w", err)
	}
	return nil
}

func (s *Store) GetTrait(ctx context.Context, traitID id.TraitID) (*trait.Trait, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(traitModel)
	q := s.sdb.NewSelect(m).Where("id = ?", traitID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	err := q.Scan(ctx)
	if err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrTraitNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get trait: %w", err)
	}
	return traitFromModel(m)
}

func (s *Store) GetTraitByName(ctx context.Context, name string) (*trait.Trait, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(traitModel)
	q := s.sdb.NewSelect(m).Where("name = ?", name)
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	err := q.Scan(ctx)
	if err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrTraitNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get trait by name: %w", err)
	}
	return traitFromModel(m)
}

// mutableTraitColumns is every cortex_traits column UpdateTrait is
// allowed to write. A trait's scope is set once at creation and never
// rewritten: scope_l0/l1/l2/extra/canon are deliberately absent here.
// Grove's NewUpdate builds SET from every model field by default, so
// without this explicit whitelist UpdateTrait would blank the scope
// columns on every call (traitToModel always derives them from
// t.Scope, and nothing populates t.Scope on the update path).
//
// app_id is deliberately absent too, for the same reason: traitToModel
// always writes AppID as "" now (Trait carries no such field to draw
// from), so including "app_id" here would blank whatever a pre-v1.8.0
// row's app_id column still holds on its very first update. The column
// itself is vestigial but intentionally left in place; erasing its
// content belongs to whatever change finally drops the column.
var mutableTraitColumns = []string{
	"name",
	"description",
	"dimensions",
	"influences",
	"category",
	"metadata",
	"created_at",
	"updated_at",
}

// UpdateTrait modifies an existing trait's mutable fields within the
// caller's scope. Scope is immutable after creation: the context scope is
// used only as an authorization predicate (the caller must be at or above
// the trait's stored scope to touch it), and is never written back —
// mutableTraitColumns excludes the five scope columns from what gets
// written.
func (s *Store) UpdateTrait(ctx context.Context, t *trait.Trait) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	t.UpdatedAt = time.Now().UTC()
	m := traitToModel(t)
	q := s.sdb.NewUpdate(m).Column(mutableTraitColumns...).WherePK()
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
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
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	q := s.sdb.NewDelete((*traitModel)(nil)).
		Where("id = ?", traitID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
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
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &trait.ListFilter{}
	}
	var models []traitModel
	q := s.sdb.NewSelect(&models).OrderExpr("created_at ASC")
	for _, p := range scopePredicates(scope, filter.Exact) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if filter.Category != "" {
		q = q.Where("category = ?", string(filter.Category))
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

func (s *Store) CountTraits(ctx context.Context, filter *trait.ListFilter) (int64, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return 0, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &trait.ListFilter{}
	}
	q := s.sdb.NewSelect((*traitModel)(nil))
	for _, p := range scopePredicates(scope, filter.Exact) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if filter.Category != "" {
		q = q.Where("category = ?", string(filter.Category))
	}
	if filter.Search != "" {
		q = q.Where("LOWER(name) LIKE LOWER(?)", "%"+filter.Search+"%")
	}
	count, err := q.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/sqlite: count traits: %w", err)
	}
	return count, nil
}
