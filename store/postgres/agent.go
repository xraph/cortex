package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
)

func (s *Store) Create(ctx context.Context, config *agent.Config) error {
	now := time.Now().UTC()
	config.CreatedAt = now
	config.UpdatedAt = now
	m := agentToModel(config)
	_, err := s.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: create agent: %w", err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, agentID id.AgentID) (*agent.Config, error) {
	m := new(agentModel)
	err := s.db.NewSelect().Model(m).Where("id = ?", agentID.String()).Scan(ctx)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, cortex.ErrAgentNotFound
		}
		return nil, fmt.Errorf("cortex: get agent: %w", err)
	}
	return agentFromModel(m), nil
}

func (s *Store) GetByName(ctx context.Context, appID, name string) (*agent.Config, error) {
	m := new(agentModel)
	err := s.db.NewSelect().Model(m).
		Where("app_id = ?", appID).
		Where("name = ?", name).
		Scan(ctx)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, cortex.ErrAgentNotFound
		}
		return nil, fmt.Errorf("cortex: get agent by name: %w", err)
	}
	return agentFromModel(m), nil
}

func (s *Store) Update(ctx context.Context, config *agent.Config) error {
	config.UpdatedAt = time.Now().UTC()
	m := agentToModel(config)
	res, err := s.db.NewUpdate().Model(m).WherePK().Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: update agent: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return cortex.ErrAgentNotFound
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, agentID id.AgentID) error {
	res, err := s.db.NewDelete().
		Model((*agentModel)(nil)).
		Where("id = ?", agentID.String()).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: delete agent: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return cortex.ErrAgentNotFound
	}
	return nil
}

func (s *Store) List(ctx context.Context, filter *agent.ListFilter) ([]*agent.Config, error) {
	var models []agentModel
	q := s.db.NewSelect().Model(&models).OrderExpr("created_at ASC")
	if filter != nil {
		if filter.AppID != "" {
			q = q.Where("app_id = ?", filter.AppID)
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
		result[i] = agentFromModel(&models[i])
	}
	return result, nil
}
