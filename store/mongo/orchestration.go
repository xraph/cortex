package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

// CreateOrchestration persists a new orchestration config, stamping the
// scope from the context.
func (s *Store) CreateOrchestration(ctx context.Context, c *orchestration.Config) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	t := now()
	c.CreatedAt = t
	c.UpdatedAt = t
	c.Scope = scope

	if _, err := s.mdb.NewInsert(orchestrationConfigToModel(c)).Exec(ctx); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/mongo: create orchestration: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex/mongo: create orchestration: %w", err)
	}

	return nil
}

// GetOrchestration returns an orchestration config by ID within the
// caller's scope.
func (s *Store) GetOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) (*orchestration.Config, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var m orchestrationConfigModel

	filter := bson.M{"_id": orchID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	err := s.mdb.NewFind(&m).
		Filter(filter).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrOrchestrationNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get orchestration: %w", err)
	}

	return orchestrationConfigFromModel(&m)
}

// GetOrchestrationByName returns an orchestration config by app ID and
// name within the caller's scope. appID stays a real, required lookup
// parameter here (unlike agent/persona, which dropped it): scope is
// layered on top of it, not a replacement for it.
func (s *Store) GetOrchestrationByName(ctx context.Context, appID, name string) (*orchestration.Config, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var m orchestrationConfigModel

	filter := bson.M{"app_id": appID, "name": name}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	err := s.mdb.NewFind(&m).
		Filter(filter).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrOrchestrationNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get orchestration by name: %w", err)
	}

	return orchestrationConfigFromModel(&m)
}

// UpdateOrchestration modifies an existing orchestration config's mutable
// fields within the caller's scope. Scope is immutable after creation:
// the context scope is used only as an authorization predicate (the
// caller must be at or above the config's stored scope to touch it), and
// is never written back. grove's NewUpdate(model).Exec defaults to a
// full-field $set built from the model struct when no explicit update
// document is given, which would otherwise blank
// scope_l0/l1/l2/extra/canon on every call.
func (s *Store) UpdateOrchestration(ctx context.Context, c *orchestration.Config) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	c.UpdatedAt = now()
	m := orchestrationConfigToModel(c)

	filter := bson.M{"_id": m.ID}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	set := bson.M{
		"name":         m.Name,
		"description":  m.Description,
		"app_id":       m.AppID,
		"strategy":     m.Strategy,
		"participants": m.Participants,
		"settings":     m.Settings,
		"metadata":     m.Metadata,
		"updated_at":   m.UpdatedAt,
	}

	res, err := s.mdb.NewUpdate((*orchestrationConfigModel)(nil)).
		Filter(filter).
		SetUpdate(bson.M{"$set": set}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update orchestration: %w", err)
	}

	if res.MatchedCount() == 0 {
		return cortex.ErrOrchestrationNotFound
	}

	return nil
}

// DeleteOrchestration removes an orchestration config within the
// caller's scope.
func (s *Store) DeleteOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	filter := bson.M{"_id": orchID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	res, err := s.mdb.NewDelete((*orchestrationConfigModel)(nil)).
		Filter(filter).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: delete orchestration: %w", err)
	}

	if res.DeletedCount() == 0 {
		return cortex.ErrOrchestrationNotFound
	}

	return nil
}

// ListOrchestrations returns orchestration configs within the caller's
// scope, optionally filtered.
func (s *Store) ListOrchestrations(ctx context.Context, filter *orchestration.ConfigListFilter) ([]*orchestration.Config, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &orchestration.ConfigListFilter{}
	}
	var models []orchestrationConfigModel

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
		return nil, fmt.Errorf("cortex/mongo: list orchestrations: %w", err)
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

// CountOrchestrations returns the total number of orchestration configs
// matching the filter within the caller's scope.
func (s *Store) CountOrchestrations(ctx context.Context, filter *orchestration.ConfigListFilter) (int64, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return 0, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &orchestration.ConfigListFilter{}
	}
	f := bson.M{}
	for k, v := range scopeFilter(scope, filter.Exact) {
		f[k] = v
	}
	if filter.Search != "" {
		f["name"] = bson.M{"$regex": filter.Search, "$options": "i"}
	}

	count, err := s.mdb.NewFind((*orchestrationConfigModel)(nil)).
		Filter(f).
		Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/mongo: count orchestrations: %w", err)
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
	t := now()
	r.CreatedAt = t
	r.UpdatedAt = t
	r.Scope = scope

	if _, err := s.mdb.NewInsert(orchestrationRunToModel(r)).Exec(ctx); err != nil {
		return fmt.Errorf("cortex/mongo: create orchestration run: %w", err)
	}

	return nil
}

// GetOrchestrationRun returns an orchestration run by ID within the
// caller's scope.
func (s *Store) GetOrchestrationRun(ctx context.Context, runID id.OrchestrationID) (*orchestration.Run, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var m orchestrationRunModel

	filter := bson.M{"_id": runID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	err := s.mdb.NewFind(&m).
		Filter(filter).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrOrchestrationRunNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get orchestration run: %w", err)
	}

	return orchestrationRunFromModel(&m)
}

// UpdateOrchestrationRun modifies an existing orchestration run's mutable
// fields within the caller's scope. Scope is immutable after creation,
// same as UpdateOrchestration above.
func (s *Store) UpdateOrchestrationRun(ctx context.Context, r *orchestration.Run) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	r.UpdatedAt = now()
	m := orchestrationRunToModel(r)

	filter := bson.M{"_id": m.ID}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	set := bson.M{
		"config_id":     m.ConfigID,
		"app_id":        m.AppID,
		"strategy":      m.Strategy,
		"status":        m.Status,
		"input":         m.Input,
		"output":        m.Output,
		"error":         m.Error,
		"agent_run_ids": m.AgentRunIDs,
		"started_at":    m.StartedAt,
		"completed_at":  m.CompletedAt,
		"updated_at":    m.UpdatedAt,
	}

	res, err := s.mdb.NewUpdate((*orchestrationRunModel)(nil)).
		Filter(filter).
		SetUpdate(bson.M{"$set": set}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update orchestration run: %w", err)
	}

	if res.MatchedCount() == 0 {
		return cortex.ErrOrchestrationRunNotFound
	}

	return nil
}

// ListOrchestrationRuns returns orchestration runs within the caller's
// scope, optionally filtered.
func (s *Store) ListOrchestrationRuns(ctx context.Context, filter *orchestration.RunListFilter) ([]*orchestration.Run, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &orchestration.RunListFilter{}
	}
	var models []orchestrationRunModel

	f := bson.M{}
	for k, v := range scopeFilter(scope, filter.Exact) {
		f[k] = v
	}
	if filter.Status != "" {
		f["status"] = filter.Status
	}

	q := s.mdb.NewFind(&models).
		Filter(f).
		Sort(bson.D{{Key: "created_at", Value: -1}})

	if filter.Limit > 0 {
		q = q.Limit(int64(filter.Limit))
	}

	if filter.Offset > 0 {
		q = q.Skip(int64(filter.Offset))
	}

	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/mongo: list orchestration runs: %w", err)
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

// CountOrchestrationRuns returns the total number of orchestration runs
// matching the filter within the caller's scope.
func (s *Store) CountOrchestrationRuns(ctx context.Context, filter *orchestration.RunListFilter) (int64, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return 0, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &orchestration.RunListFilter{}
	}
	f := bson.M{}
	for k, v := range scopeFilter(scope, filter.Exact) {
		f[k] = v
	}
	if filter.Status != "" {
		f["status"] = filter.Status
	}

	count, err := s.mdb.NewFind((*orchestrationRunModel)(nil)).
		Filter(f).
		Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/mongo: count orchestration runs: %w", err)
	}

	return count, nil
}
