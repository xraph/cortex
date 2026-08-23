package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/persona"
)

// CreatePersona persists a new persona, stamping the scope from the
// context. UNIQUE (scope_canon, name) is what actually enforces
// per-scope name uniqueness; app_id survives on the row as a vestigial
// column no longer read or filtered on.
func (s *Store) CreatePersona(ctx context.Context, p *persona.Persona) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	now := time.Now().UTC()
	p.CreatedAt = now
	p.UpdatedAt = now
	p.Scope = scope
	m := personaToModel(p)
	_, err := s.sdb.NewInsert(m).Exec(ctx)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/sqlite: create persona: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex/sqlite: create persona: %w", err)
	}
	return nil
}

func (s *Store) GetPersona(ctx context.Context, personaID id.PersonaID) (*persona.Persona, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(personaModel)
	q := s.sdb.NewSelect(m).Where("id = ?", personaID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	err := q.Scan(ctx)
	if err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrPersonaNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get persona: %w", err)
	}
	return personaFromModel(m)
}

func (s *Store) GetPersonaByName(ctx context.Context, name string) (*persona.Persona, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(personaModel)
	q := s.sdb.NewSelect(m).Where("name = ?", name)
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	err := q.Scan(ctx)
	if err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrPersonaNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get persona by name: %w", err)
	}
	return personaFromModel(m)
}

// mutablePersonaColumns is every cortex_personas column UpdatePersona is
// allowed to write. A persona's scope is set once at creation and never
// rewritten: scope_l0/l1/l2/extra/canon are deliberately absent here.
// Grove's NewUpdate builds SET from every model field by default, so
// without this explicit whitelist UpdatePersona would blank the scope
// columns on every call (personaToModel always derives them from
// p.Scope, and nothing populates p.Scope on the update path).
//
// app_id is deliberately absent too, for the same reason: personaToModel
// always writes AppID as "" now (Persona carries no such field to draw
// from), so including "app_id" here would blank whatever a pre-v1.8.0
// row's app_id column still holds on its very first update. The column
// itself is vestigial but intentionally left in place; erasing its
// content is not this task's call to make.
var mutablePersonaColumns = []string{
	"name",
	"description",
	"identity",
	"skills",
	"traits",
	"behaviors",
	"cognitive_style",
	"communication_style",
	"perception",
	"metadata",
	"created_at",
	"updated_at",
}

// UpdatePersona modifies an existing persona's mutable fields within the
// caller's scope. Scope is immutable after creation: the context scope is
// used only as an authorization predicate (the caller must be at or above
// the persona's stored scope to touch it), and is never written back —
// mutablePersonaColumns excludes the five scope columns from what gets
// written.
func (s *Store) UpdatePersona(ctx context.Context, p *persona.Persona) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	p.UpdatedAt = time.Now().UTC()
	m := personaToModel(p)
	q := s.sdb.NewUpdate(m).Column(mutablePersonaColumns...).WherePK()
	for _, pr := range scopePredicates(scope, false) {
		q = q.Where(pr.Column+" = ?", pr.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: update persona: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex/sqlite: update persona rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrPersonaNotFound
	}
	return nil
}

func (s *Store) DeletePersona(ctx context.Context, personaID id.PersonaID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	q := s.sdb.NewDelete((*personaModel)(nil)).
		Where("id = ?", personaID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: delete persona: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex/sqlite: delete persona rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrPersonaNotFound
	}
	return nil
}

func (s *Store) ListPersonas(ctx context.Context, filter *persona.ListFilter) ([]*persona.Persona, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &persona.ListFilter{}
	}
	var models []personaModel
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
		return nil, fmt.Errorf("cortex/sqlite: list personas: %w", err)
	}
	result := make([]*persona.Persona, len(models))
	for i := range models {
		p, convErr := personaFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = p
	}
	return result, nil
}

func (s *Store) CountPersonas(ctx context.Context, filter *persona.ListFilter) (int64, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return 0, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &persona.ListFilter{}
	}
	q := s.sdb.NewSelect((*personaModel)(nil))
	for _, p := range scopePredicates(scope, filter.Exact) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if filter.Search != "" {
		q = q.Where("LOWER(name) LIKE LOWER(?)", "%"+filter.Search+"%")
	}
	count, err := q.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/sqlite: count personas: %w", err)
	}
	return count, nil
}
