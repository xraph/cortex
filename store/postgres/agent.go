package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
)

// Create persists a new agent config, stamping the scope from the
// context. UNIQUE (scope_canon, name) is what actually enforces
// per-scope name uniqueness; app_id survives on the row as a vestigial
// column no longer read or filtered on.
func (s *Store) Create(ctx context.Context, config *agent.Config) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	now := time.Now().UTC()
	config.CreatedAt = now
	config.UpdatedAt = now
	config.Scope = scope
	m := agentToModel(config)
	_, err := s.pgdb.NewInsert(m).Exec(ctx)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex: create agent: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex: create agent: %w", err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, agentID id.AgentID) (*agent.Config, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(agentModel)
	q := s.pgdb.NewSelect(m).Where("id = ?", agentID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	err := q.Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, cortex.ErrAgentNotFound
		}
		return nil, fmt.Errorf("cortex: get agent: %w", err)
	}
	return agentFromModel(m)
}

func (s *Store) GetByName(ctx context.Context, name string) (*agent.Config, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(agentModel)
	q := s.pgdb.NewSelect(m).Where("name = ?", name)
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	err := q.Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, cortex.ErrAgentNotFound
		}
		return nil, fmt.Errorf("cortex: get agent by name: %w", err)
	}
	return agentFromModel(m)
}

// mutableAgentColumns is every cortex_agents column Update is allowed to
// write. An agent's scope is set once at creation and never rewritten:
// scope_l0/l1/l2/extra/canon are deliberately absent here. Grove's
// NewUpdate builds SET from every model field by default, so without this
// explicit whitelist Update would blank the scope columns on every call
// (agentToModel always derives them from config.Scope, and nothing
// populates config.Scope on the Update path).
//
// app_id is deliberately absent too, for the same reason: agentToModel
// always writes AppID as "" now (Config carries no such field to draw
// from), so including "app_id" here would blank whatever a pre-v1.8.0
// row's app_id column still holds on its very first Update. The column
// itself is vestigial but intentionally left in place; erasing its
// content belongs to whatever change finally drops the column.
var mutableAgentColumns = []string{
	"name",
	"description",
	"system_prompt",
	"model",
	"tools",
	"max_steps",
	"max_tokens",
	"temperature",
	"reasoning_loop",
	"guardrails",
	"metadata",
	"enabled",
	"persona_ref",
	"inline_skills",
	"inline_traits",
	"inline_behaviors",
	"created_at",
	"updated_at",
}

// Update modifies an existing agent config's mutable fields within the
// caller's scope. Scope is immutable after creation: the context scope is
// used only as an authorization predicate (the caller must be at or above
// the agent's stored scope to touch it), and is never written back —
// mutableAgentColumns excludes the five scope columns from what gets
// written.
func (s *Store) Update(ctx context.Context, config *agent.Config) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	config.UpdatedAt = time.Now().UTC()
	m := agentToModel(config)
	q := s.pgdb.NewUpdate(m).Column(mutableAgentColumns...).WherePK()
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: update agent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cortex: update agent rows affected: %w", err)
	}
	if n == 0 {
		return cortex.ErrAgentNotFound
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, agentID id.AgentID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	q := s.pgdb.NewDelete((*agentModel)(nil)).
		Where("id = ?", agentID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: delete agent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cortex: delete agent rows affected: %w", err)
	}
	if n == 0 {
		return cortex.ErrAgentNotFound
	}
	return nil
}

func (s *Store) List(ctx context.Context, filter *agent.ListFilter) ([]*agent.Config, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &agent.ListFilter{}
	}
	var models []agentModel
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
		return nil, fmt.Errorf("cortex: list agents: %w", err)
	}
	result := make([]*agent.Config, len(models))
	for i := range models {
		c, err := agentFromModel(&models[i])
		if err != nil {
			return nil, fmt.Errorf("cortex: list agents: %w", err)
		}
		result[i] = c
	}
	return result, nil
}

func (s *Store) CountAgents(ctx context.Context, filter *agent.ListFilter) (int64, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return 0, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &agent.ListFilter{}
	}
	q := s.pgdb.NewSelect((*agentModel)(nil))
	for _, p := range scopePredicates(scope, filter.Exact) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if filter.Search != "" {
		q = q.Where("LOWER(name) LIKE LOWER(?)", "%"+filter.Search+"%")
	}
	count, err := q.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex: count agents: %w", err)
	}
	return count, nil
}
