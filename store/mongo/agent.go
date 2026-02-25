package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
)

// Create persists a new agent configuration.
func (s *Store) Create(ctx context.Context, config *agent.Config) error {
	t := now()
	config.CreatedAt = t
	config.UpdatedAt = t
	m := agentToModel(config)

	_, err := s.mdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: create agent: %w", err)
	}

	return nil
}

// Get returns an agent configuration by ID.
func (s *Store) Get(ctx context.Context, agentID id.AgentID) (*agent.Config, error) {
	var m agentModel

	err := s.mdb.NewFind(&m).
		Filter(bson.M{"_id": agentID.String()}).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrAgentNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get agent: %w", err)
	}

	return agentFromModel(&m)
}

// GetByName returns an agent by app ID and name.
func (s *Store) GetByName(ctx context.Context, appID, name string) (*agent.Config, error) {
	var m agentModel

	err := s.mdb.NewFind(&m).
		Filter(bson.M{"app_id": appID, "name": name}).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrAgentNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get agent by name: %w", err)
	}

	return agentFromModel(&m)
}

// Update modifies an existing agent configuration.
func (s *Store) Update(ctx context.Context, config *agent.Config) error {
	config.UpdatedAt = now()
	m := agentToModel(config)

	res, err := s.mdb.NewUpdate(m).
		Filter(bson.M{"_id": m.ID}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update agent: %w", err)
	}

	if res.MatchedCount() == 0 {
		return cortex.ErrAgentNotFound
	}

	return nil
}

// Delete removes an agent configuration.
func (s *Store) Delete(ctx context.Context, agentID id.AgentID) error {
	res, err := s.mdb.NewDelete((*agentModel)(nil)).
		Filter(bson.M{"_id": agentID.String()}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: delete agent: %w", err)
	}

	if res.DeletedCount() == 0 {
		return cortex.ErrAgentNotFound
	}

	return nil
}

// List returns agent configurations, optionally filtered.
func (s *Store) List(ctx context.Context, filter *agent.ListFilter) ([]*agent.Config, error) {
	var models []agentModel

	f := bson.M{}
	if filter != nil && filter.AppID != "" {
		f["app_id"] = filter.AppID
	}

	q := s.mdb.NewFind(&models).
		Filter(f).
		Sort(bson.D{{Key: "created_at", Value: 1}})

	if filter != nil {
		if filter.Limit > 0 {
			q = q.Limit(int64(filter.Limit))
		}

		if filter.Offset > 0 {
			q = q.Skip(int64(filter.Offset))
		}
	}

	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/mongo: list agents: %w", err)
	}

	result := make([]*agent.Config, len(models))
	for i := range models {
		c, convErr := agentFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = c
	}

	return result, nil
}
