package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

// CreateOrchestration persists a new orchestration config.
func (s *Store) CreateOrchestration(ctx context.Context, c *orchestration.Config) error {
	t := now()
	c.CreatedAt = t
	c.UpdatedAt = t

	if _, err := s.mdb.NewInsert(orchestrationConfigToModel(c)).Exec(ctx); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/mongo: create orchestration: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex/mongo: create orchestration: %w", err)
	}

	return nil
}

// GetOrchestration returns an orchestration config by ID.
func (s *Store) GetOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) (*orchestration.Config, error) {
	var m orchestrationConfigModel

	err := s.mdb.NewFind(&m).
		Filter(bson.M{"_id": orchID.String()}).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrOrchestrationNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get orchestration: %w", err)
	}

	return orchestrationConfigFromModel(&m)
}

// GetOrchestrationByName returns an orchestration config by app ID and name.
func (s *Store) GetOrchestrationByName(ctx context.Context, appID, name string) (*orchestration.Config, error) {
	var m orchestrationConfigModel

	err := s.mdb.NewFind(&m).
		Filter(bson.M{"app_id": appID, "name": name}).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrOrchestrationNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get orchestration by name: %w", err)
	}

	return orchestrationConfigFromModel(&m)
}

// UpdateOrchestration modifies an existing orchestration config.
func (s *Store) UpdateOrchestration(ctx context.Context, c *orchestration.Config) error {
	c.UpdatedAt = now()
	m := orchestrationConfigToModel(c)

	res, err := s.mdb.NewUpdate(m).
		Filter(bson.M{"_id": m.ID}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update orchestration: %w", err)
	}

	if res.MatchedCount() == 0 {
		return cortex.ErrOrchestrationNotFound
	}

	return nil
}

// DeleteOrchestration removes an orchestration config.
func (s *Store) DeleteOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) error {
	res, err := s.mdb.NewDelete((*orchestrationConfigModel)(nil)).
		Filter(bson.M{"_id": orchID.String()}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: delete orchestration: %w", err)
	}

	if res.DeletedCount() == 0 {
		return cortex.ErrOrchestrationNotFound
	}

	return nil
}

// ListOrchestrations returns orchestration configs, optionally filtered.
func (s *Store) ListOrchestrations(ctx context.Context, filter *orchestration.ConfigListFilter) ([]*orchestration.Config, error) {
	var models []orchestrationConfigModel

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

// CountOrchestrations returns the total number of orchestration configs matching the filter.
func (s *Store) CountOrchestrations(ctx context.Context, filter *orchestration.ConfigListFilter) (int64, error) {
	f := bson.M{}
	if filter != nil {
		if filter.AppID != "" {
			f["app_id"] = filter.AppID
		}

		if filter.Search != "" {
			f["name"] = bson.M{"$regex": filter.Search, "$options": "i"}
		}
	}

	count, err := s.mdb.NewFind((*orchestrationConfigModel)(nil)).
		Filter(f).
		Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/mongo: count orchestrations: %w", err)
	}

	return count, nil
}

// CreateOrchestrationRun persists a new orchestration run.
func (s *Store) CreateOrchestrationRun(ctx context.Context, r *orchestration.Run) error {
	t := now()
	r.CreatedAt = t
	r.UpdatedAt = t

	if _, err := s.mdb.NewInsert(orchestrationRunToModel(r)).Exec(ctx); err != nil {
		return fmt.Errorf("cortex/mongo: create orchestration run: %w", err)
	}

	return nil
}

// GetOrchestrationRun returns an orchestration run by ID.
func (s *Store) GetOrchestrationRun(ctx context.Context, runID id.OrchestrationID) (*orchestration.Run, error) {
	var m orchestrationRunModel

	err := s.mdb.NewFind(&m).
		Filter(bson.M{"_id": runID.String()}).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrOrchestrationRunNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get orchestration run: %w", err)
	}

	return orchestrationRunFromModel(&m)
}

// UpdateOrchestrationRun modifies an existing orchestration run.
func (s *Store) UpdateOrchestrationRun(ctx context.Context, r *orchestration.Run) error {
	r.UpdatedAt = now()
	m := orchestrationRunToModel(r)

	res, err := s.mdb.NewUpdate(m).
		Filter(bson.M{"_id": m.ID}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update orchestration run: %w", err)
	}

	if res.MatchedCount() == 0 {
		return cortex.ErrOrchestrationRunNotFound
	}

	return nil
}

// ListOrchestrationRuns returns orchestration runs, optionally filtered.
func (s *Store) ListOrchestrationRuns(ctx context.Context, filter *orchestration.RunListFilter) ([]*orchestration.Run, error) {
	var models []orchestrationRunModel

	f := bson.M{}
	if filter != nil {
		if filter.AppID != "" {
			f["app_id"] = filter.AppID
		}

		if filter.Status != "" {
			f["status"] = filter.Status
		}
	}

	q := s.mdb.NewFind(&models).
		Filter(f).
		Sort(bson.D{{Key: "created_at", Value: -1}})

	if filter != nil {
		if filter.Limit > 0 {
			q = q.Limit(int64(filter.Limit))
		}

		if filter.Offset > 0 {
			q = q.Skip(int64(filter.Offset))
		}
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

// CountOrchestrationRuns returns the total number of orchestration runs matching the filter.
func (s *Store) CountOrchestrationRuns(ctx context.Context, filter *orchestration.RunListFilter) (int64, error) {
	f := bson.M{}
	if filter != nil {
		if filter.AppID != "" {
			f["app_id"] = filter.AppID
		}

		if filter.Status != "" {
			f["status"] = filter.Status
		}
	}

	count, err := s.mdb.NewFind((*orchestrationRunModel)(nil)).
		Filter(f).
		Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/mongo: count orchestration runs: %w", err)
	}

	return count, nil
}
