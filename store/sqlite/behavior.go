package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/id"
)

// CreateBehavior persists a new behavior, stamping the scope from the
// context. UNIQUE (scope_canon, name) is what actually enforces
// per-scope name uniqueness; app_id survives on the row as a vestigial
// column no longer read or filtered on.
func (s *Store) CreateBehavior(ctx context.Context, b *behavior.Behavior) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	now := time.Now().UTC()
	b.CreatedAt = now
	b.UpdatedAt = now
	b.Scope = scope
	m := behaviorToModel(b)
	_, err := s.sdb.NewInsert(m).Exec(ctx)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/sqlite: create behavior: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex/sqlite: create behavior: %w", err)
	}
	return nil
}

func (s *Store) GetBehavior(ctx context.Context, behaviorID id.BehaviorID) (*behavior.Behavior, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(behaviorModel)
	q := s.sdb.NewSelect(m).Where("id = ?", behaviorID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	err := q.Scan(ctx)
	if err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrBehaviorNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get behavior: %w", err)
	}
	return behaviorFromModel(m)
}

func (s *Store) GetBehaviorByName(ctx context.Context, name string) (*behavior.Behavior, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(behaviorModel)
	q := s.sdb.NewSelect(m).Where("name = ?", name)
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	err := q.Scan(ctx)
	if err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrBehaviorNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get behavior by name: %w", err)
	}
	return behaviorFromModel(m)
}

// mutableBehaviorColumns is every cortex_behaviors column UpdateBehavior
// is allowed to write. A behavior's scope is set once at creation and
// never rewritten: scope_l0/l1/l2/extra/canon are deliberately absent
// here. Grove's NewUpdate builds SET from every model field by default,
// so without this explicit whitelist UpdateBehavior would blank the
// scope columns on every call (behaviorToModel always derives them from
// b.Scope, and nothing populates b.Scope on the update path).
//
// app_id is deliberately absent too, for the same reason: behaviorToModel
// always writes AppID as "" now (Behavior carries no such field to draw
// from), so including "app_id" here would blank whatever a pre-v1.8.0
// row's app_id column still holds on its very first update. The column
// itself is vestigial but intentionally left in place; erasing its
// content belongs to whatever change finally drops the column.
var mutableBehaviorColumns = []string{
	"name",
	"description",
	"triggers",
	"actions",
	"priority",
	"requires_skill",
	"requires_trait",
	"metadata",
	"created_at",
	"updated_at",
}

// UpdateBehavior modifies an existing behavior's mutable fields within
// the caller's scope. Scope is immutable after creation: the context
// scope is used only as an authorization predicate (the caller must be
// at or above the behavior's stored scope to touch it), and is never
// written back — mutableBehaviorColumns excludes the five scope columns
// from what gets written.
func (s *Store) UpdateBehavior(ctx context.Context, b *behavior.Behavior) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	b.UpdatedAt = time.Now().UTC()
	m := behaviorToModel(b)
	q := s.sdb.NewUpdate(m).Column(mutableBehaviorColumns...).WherePK()
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
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
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	q := s.sdb.NewDelete((*behaviorModel)(nil)).
		Where("id = ?", behaviorID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
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
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &behavior.ListFilter{}
	}
	var models []behaviorModel
	q := s.sdb.NewSelect(&models).OrderExpr("created_at ASC")
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

func (s *Store) CountBehaviors(ctx context.Context, filter *behavior.ListFilter) (int64, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return 0, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &behavior.ListFilter{}
	}
	q := s.sdb.NewSelect((*behaviorModel)(nil))
	for _, p := range scopePredicates(scope, filter.Exact) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if filter.Search != "" {
		q = q.Where("LOWER(name) LIKE LOWER(?)", "%"+filter.Search+"%")
	}
	count, err := q.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/sqlite: count behaviors: %w", err)
	}
	return count, nil
}
