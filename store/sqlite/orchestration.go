package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

// mutableOrchestrationConfigColumns is every cortex_orchestration_configs
// column UpdateOrchestration is allowed to write. A config's scope is set
// once at creation and never rewritten: scope_l0/l1/l2/extra/canon are
// deliberately absent here. Grove's NewUpdate builds SET from every model
// field by default, so without this explicit whitelist an
// UpdateOrchestration issued from a broader (but still scope-matching)
// context would silently overwrite a row's narrower stored scope.
//
// app_id is deliberately absent too, mirroring persona's
// mutablePersonaColumns: orchestration.Config dropped its AppID field
// (fix round 1 — it never governed anything once the unique index went
// scope-keyed, so keeping it live was a regression, not belt-and-braces),
// so orchestrationConfigToModel always writes AppID as "" now, and
// including "app_id" here would blank whatever a pre-fix row's app_id
// column still holds on its very first update. The column itself is
// vestigial but intentionally left in place; erasing its content belongs
// to whatever change finally drops the column.
var mutableOrchestrationConfigColumns = []string{
	"name",
	"description",
	"strategy",
	"participants",
	"settings",
	"metadata",
	"created_at",
	"updated_at",
}

// mutableOrchestrationRunColumns mirrors mutableOrchestrationConfigColumns
// for cortex_orchestration_runs: everything except the five scope columns
// and (for the same reason as above) app_id.
var mutableOrchestrationRunColumns = []string{
	"config_id",
	"strategy",
	"status",
	"input",
	"output",
	"error",
	"agent_run_ids",
	"started_at",
	"completed_at",
	"created_at",
	"updated_at",
}

// CreateOrchestration persists a new orchestration config, stamping the
// scope from the context.
func (s *Store) CreateOrchestration(ctx context.Context, c *orchestration.Config) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now
	c.Scope = scope
	if _, err := s.sdb.NewInsert(orchestrationConfigToModel(c)).Exec(ctx); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/sqlite: create orchestration: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex/sqlite: create orchestration: %w", err)
	}
	return nil
}

func (s *Store) GetOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) (*orchestration.Config, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(orchestrationConfigModel)
	q := s.sdb.NewSelect(m).Where("id = ?", orchID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err := q.Scan(ctx); err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrOrchestrationNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get orchestration: %w", err)
	}
	return orchestrationConfigFromModel(m)
}

// GetOrchestrationByName returns an orchestration config by name within
// the caller's scope. Fix round 1 dropped the appID parameter this
// method used to also filter on: with UNIQUE (scope_canon, name), at
// most one row can ever exist per (scope, name), so an app_id predicate
// on top could only ever turn a hit into a miss, never disambiguate two
// rows — the same reasoning that dropped AppID from agent and persona.
func (s *Store) GetOrchestrationByName(ctx context.Context, name string) (*orchestration.Config, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(orchestrationConfigModel)
	q := s.sdb.NewSelect(m).Where("name = ?", name)
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	err := q.Scan(ctx)
	if err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrOrchestrationNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get orchestration by name: %w", err)
	}
	return orchestrationConfigFromModel(m)
}

// UpdateOrchestration modifies an existing orchestration config's mutable
// fields within the caller's scope. Scope is immutable after creation:
// the context scope is used only as an authorization predicate (the
// caller must be at or above the config's stored scope to touch it), and
// is never written back — mutableOrchestrationConfigColumns excludes the
// five scope columns from what gets written.
func (s *Store) UpdateOrchestration(ctx context.Context, c *orchestration.Config) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	c.UpdatedAt = time.Now().UTC()
	m := orchestrationConfigToModel(c)
	q := s.sdb.NewUpdate(m).Column(mutableOrchestrationConfigColumns...).WherePK()
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: update orchestration: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex/sqlite: update orchestration rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrOrchestrationNotFound
	}
	return nil
}

func (s *Store) DeleteOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	q := s.sdb.NewDelete((*orchestrationConfigModel)(nil)).Where("id = ?", orchID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: delete orchestration: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex/sqlite: delete orchestration rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrOrchestrationNotFound
	}
	return nil
}

func (s *Store) ListOrchestrations(ctx context.Context, filter *orchestration.ConfigListFilter) ([]*orchestration.Config, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &orchestration.ConfigListFilter{}
	}
	var models []orchestrationConfigModel
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
		return nil, fmt.Errorf("cortex/sqlite: list orchestrations: %w", err)
	}
	result := make([]*orchestration.Config, len(models))
	for i := range models {
		c, convErr := orchestrationConfigFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = c
	}
	return result, nil
}

func (s *Store) CountOrchestrations(ctx context.Context, filter *orchestration.ConfigListFilter) (int64, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return 0, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &orchestration.ConfigListFilter{}
	}
	q := s.sdb.NewSelect((*orchestrationConfigModel)(nil))
	for _, p := range scopePredicates(scope, filter.Exact) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if filter.Search != "" {
		q = q.Where("LOWER(name) LIKE LOWER(?)", "%"+filter.Search+"%")
	}
	count, err := q.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/sqlite: count orchestrations: %w", err)
	}
	return count, nil
}

// CreateOrchestrationRun persists a new orchestration run, stamping the
// scope from the context.
func (s *Store) CreateOrchestrationRun(ctx context.Context, r *orchestration.Run) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	r.Scope = scope
	if _, err := s.sdb.NewInsert(orchestrationRunToModel(r)).Exec(ctx); err != nil {
		return fmt.Errorf("cortex/sqlite: create orchestration run: %w", err)
	}
	return nil
}

func (s *Store) GetOrchestrationRun(ctx context.Context, runID id.OrchestrationID) (*orchestration.Run, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(orchestrationRunModel)
	q := s.sdb.NewSelect(m).Where("id = ?", runID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err := q.Scan(ctx); err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrOrchestrationRunNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get orchestration run: %w", err)
	}
	return orchestrationRunFromModel(m)
}

// UpdateOrchestrationRun modifies an existing orchestration run's mutable
// fields within the caller's scope. Scope is immutable after creation,
// same as UpdateOrchestration above.
func (s *Store) UpdateOrchestrationRun(ctx context.Context, r *orchestration.Run) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	r.UpdatedAt = time.Now().UTC()
	m := orchestrationRunToModel(r)
	q := s.sdb.NewUpdate(m).Column(mutableOrchestrationRunColumns...).WherePK()
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: update orchestration run: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex/sqlite: update orchestration run rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrOrchestrationRunNotFound
	}
	return nil
}

func (s *Store) ListOrchestrationRuns(ctx context.Context, filter *orchestration.RunListFilter) ([]*orchestration.Run, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &orchestration.RunListFilter{}
	}
	var models []orchestrationRunModel
	q := s.sdb.NewSelect(&models).OrderExpr("created_at DESC")
	for _, p := range scopePredicates(scope, filter.Exact) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if filter.Status != "" {
		q = q.Where("status = ?", filter.Status)
	}
	if filter.Limit > 0 {
		q = q.Limit(filter.Limit)
	}
	if filter.Offset > 0 {
		q = q.Offset(filter.Offset)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/sqlite: list orchestration runs: %w", err)
	}
	result := make([]*orchestration.Run, len(models))
	for i := range models {
		r, convErr := orchestrationRunFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = r
	}
	return result, nil
}

func (s *Store) CountOrchestrationRuns(ctx context.Context, filter *orchestration.RunListFilter) (int64, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return 0, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &orchestration.RunListFilter{}
	}
	q := s.sdb.NewSelect((*orchestrationRunModel)(nil))
	for _, p := range scopePredicates(scope, filter.Exact) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if filter.Status != "" {
		q = q.Where("status = ?", filter.Status)
	}
	count, err := q.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/sqlite: count orchestration runs: %w", err)
	}
	return count, nil
}
