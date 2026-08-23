package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
)

// Create persists a new agent configuration, stamping the scope from the
// context. The (scope_canon, name) unique index is what actually
// enforces per-scope name uniqueness; app_id survives on the document as
// a vestigial field no longer read or filtered on.
func (s *Store) Create(ctx context.Context, config *agent.Config) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	t := now()
	config.CreatedAt = t
	config.UpdatedAt = t
	config.Scope = scope
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

// Get returns an agent configuration by ID within the caller's scope.
func (s *Store) Get(ctx context.Context, agentID id.AgentID) (*agent.Config, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var m agentModel

	filter := bson.M{"_id": agentID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	err := s.mdb.NewFind(&m).
		Filter(filter).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrAgentNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get agent: %w", err)
	}

	return agentFromModel(&m)
}

// GetByName returns an agent by name within the caller's scope.
func (s *Store) GetByName(ctx context.Context, name string) (*agent.Config, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var m agentModel

	filter := bson.M{"name": name}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	err := s.mdb.NewFind(&m).
		Filter(filter).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrAgentNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get agent by name: %w", err)
	}

	return agentFromModel(&m)
}

// Update modifies an existing agent configuration's mutable fields within
// the caller's scope. Scope is immutable after creation: the context
// scope is used only as an authorization predicate (the caller must be at
// or above the agent's stored scope to touch it), and is never written
// back. grove's NewUpdate(model).Exec defaults to a full-field $set built
// from the model struct when no explicit update document is given
// (confirmed by reading mongodriver's query_update.go), which would
// otherwise blank scope_l0/l1/l2/extra/canon on every call.
//
// app_id is deliberately absent from set too, for the same reason:
// agentToModel always writes AppID as "" now (Config carries no such
// field to draw from), so including it here would blank whatever a
// pre-v1.8.0 document's app_id field still holds on its very first
// Update. The field itself is vestigial but intentionally left in place;
// erasing its content is not this task's call to make.
func (s *Store) Update(ctx context.Context, config *agent.Config) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	config.UpdatedAt = now()
	m := agentToModel(config)

	filter := bson.M{"_id": m.ID}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	set := bson.M{
		"name":             m.Name,
		"description":      m.Description,
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
		Filter(filter).
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

// Delete removes an agent configuration within the caller's scope.
func (s *Store) Delete(ctx context.Context, agentID id.AgentID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	filter := bson.M{"_id": agentID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	res, err := s.mdb.NewDelete((*agentModel)(nil)).
		Filter(filter).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: delete agent: %w", err)
	}

	if res.DeletedCount() == 0 {
		return cortex.ErrAgentNotFound
	}

	return nil
}

// List returns agent configurations within the caller's scope, optionally
// filtered.
func (s *Store) List(ctx context.Context, filter *agent.ListFilter) ([]*agent.Config, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &agent.ListFilter{}
	}
	var models []agentModel

	f := bson.M{}
	for k, v := range scopeFilter(scope, filter.Exact) {
		f[k] = v
	}
	if filter.Search != "" {
		f["name"] = bson.M{"$regex": filter.Search, "$options": "i"}
	}

	q := s.mdb.NewFind(&models).
		Filter(f).
		Sort(bson.D{{Key: "created_at", Value: 1}})

	if filter.Limit > 0 {
		q = q.Limit(int64(filter.Limit))
	}

	if filter.Offset > 0 {
		q = q.Skip(int64(filter.Offset))
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

// CountAgents returns the total number of agents matching the filter
// within the caller's scope.
func (s *Store) CountAgents(ctx context.Context, filter *agent.ListFilter) (int64, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return 0, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &agent.ListFilter{}
	}
	f := bson.M{}
	for k, v := range scopeFilter(scope, filter.Exact) {
		f[k] = v
	}
	if filter.Search != "" {
		f["name"] = bson.M{"$regex": filter.Search, "$options": "i"}
	}

	count, err := s.mdb.NewFind((*agentModel)(nil)).
		Filter(f).
		Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/mongo: count agents: %w", err)
	}

	return count, nil
}
