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

// Create persists a new agent config. The agent store stays app-keyed this
// phase (see List/Get/Update/Delete below), but the row still records
// whatever scope is on the context so the columns are populated for the
// day agent reads become scope-filtered too.
func (s *Store) Create(ctx context.Context, config *agent.Config) error {
	now := time.Now().UTC()
	config.CreatedAt = now
	config.UpdatedAt = now
	config.Scope = cortex.ScopeFromContext(ctx)
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
	m := new(agentModel)
	err := s.pgdb.NewSelect(m).Where("id = ?", agentID.String()).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, cortex.ErrAgentNotFound
		}
		return nil, fmt.Errorf("cortex: get agent: %w", err)
	}
	return agentFromModel(m)
}

func (s *Store) GetByName(ctx context.Context, appID, name string) (*agent.Config, error) {
	m := new(agentModel)
	err := s.pgdb.NewSelect(m).
		Where("app_id = ?", appID).
		Where("name = ?", name).
		Scan(ctx)
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
var mutableAgentColumns = []string{
	"name",
	"description",
	"app_id",
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

// Update modifies an existing agent config. The agent store stays
// app-keyed this phase — no scope predicate is added here, Update locates
// the row exactly as it always has (by primary key). Only the write side
// changes: scope is immutable after creation, so mutableAgentColumns
// excludes the five scope columns from what gets written.
func (s *Store) Update(ctx context.Context, config *agent.Config) error {
	config.UpdatedAt = time.Now().UTC()
	m := agentToModel(config)
	res, err := s.pgdb.NewUpdate(m).Column(mutableAgentColumns...).WherePK().Exec(ctx)
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
	res, err := s.pgdb.NewDelete((*agentModel)(nil)).
		Where("id = ?", agentID.String()).
		Exec(ctx)
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
	var models []agentModel
	q := s.pgdb.NewSelect(&models).OrderExpr("created_at ASC")
	if filter != nil {
		if filter.AppID != "" {
			q = q.Where("app_id = ?", filter.AppID)
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
	q := s.pgdb.NewSelect((*agentModel)(nil))
	if filter != nil {
		if filter.AppID != "" {
			q = q.Where("app_id = ?", filter.AppID)
		}
		if filter.Search != "" {
			q = q.Where("LOWER(name) LIKE LOWER(?)", "%"+filter.Search+"%")
		}
	}
	count, err := q.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex: count agents: %w", err)
	}
	return count, nil
}
