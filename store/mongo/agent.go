package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
)

// Create persists a new agent configuration. The agent store stays
// app-keyed this phase (see Get/Update/Delete/List below), but the
// document still records whatever scope is on the context so the fields
// are populated for the day agent reads become scope-filtered too.
func (s *Store) Create(ctx context.Context, config *agent.Config) error {
	t := now()
	config.CreatedAt = t
	config.UpdatedAt = t
	config.Scope = cortex.ScopeFromContext(ctx)
	m := agentToModel(config)

	_, err := s.mdb.NewInsert(m).Exec(ctx)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/mongo: create agent: %w", cortex.ErrAlreadyExists)
		}
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

// Update modifies an existing agent configuration's mutable fields. The
// agent store stays app-keyed this phase — no scope filter is added here,
// Update locates the document exactly as it always has (by _id). Only the
// write side changes: scope is immutable after creation, so this builds
// an explicit $set instead of passing the model through. grove's
// NewUpdate(model).Exec defaults to a full-field $set built from the
// model struct when no explicit update document is given (confirmed by
// reading mongodriver's query_update.go), which would otherwise blank
// scope_l0/l1/l2/extra/canon on every call.
func (s *Store) Update(ctx context.Context, config *agent.Config) error {
	config.UpdatedAt = now()
	m := agentToModel(config)

	set := bson.M{
		"name":             m.Name,
		"description":      m.Description,
		"app_id":           m.AppID,
		"system_prompt":    m.SystemPrompt,
		"model":            m.Model,
		"tools":            m.Tools,
		"max_steps":        m.MaxSteps,
		"max_tokens":       m.MaxTokens,
		"temperature":      m.Temperature,
		"reasoning_loop":   m.ReasoningLoop,
		"guardrails":       m.Guardrails,
		"metadata":         m.Metadata,
		"enabled":          m.Enabled,
		"persona_ref":      m.PersonaRef,
		"inline_skills":    m.InlineSkills,
		"inline_traits":    m.InlineTraits,
		"inline_behaviors": m.InlineBehaviors,
		"updated_at":       m.UpdatedAt,
	}

	res, err := s.mdb.NewUpdate((*agentModel)(nil)).
		Filter(bson.M{"_id": m.ID}).
		SetUpdate(bson.M{"$set": set}).
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
	if filter != nil {
		if filter.AppID != "" {
			f["app_id"] = filter.AppID
		}

		if filter.Search != "" {
			f["name"] = bson.M{"$regex": filter.Search, "$options": "i"}
		}
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

// CountAgents returns the total number of agents matching the filter.
func (s *Store) CountAgents(ctx context.Context, filter *agent.ListFilter) (int64, error) {
	f := bson.M{}
	if filter != nil {
		if filter.AppID != "" {
			f["app_id"] = filter.AppID
		}

		if filter.Search != "" {
			f["name"] = bson.M{"$regex": filter.Search, "$options": "i"}
		}
	}

	count, err := s.mdb.NewFind((*agentModel)(nil)).
		Filter(f).
		Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/mongo: count agents: %w", err)
	}

	return count, nil
}
