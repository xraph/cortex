package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func (s *Store) CreateOrchestration(ctx context.Context, c *orchestration.OrchestrationConfig) error {
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now
	if _, err := s.pgdb.NewInsert(orchestrationConfigToModel(c)).Exec(ctx); err != nil {
		return fmt.Errorf("cortex: create orchestration: %w", err)
	}
	return nil
}

func (s *Store) GetOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) (*orchestration.OrchestrationConfig, error) {
	m := new(orchestrationConfigModel)
	if err := s.pgdb.NewSelect(m).Where("id = ?", orchID.String()).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, cortex.ErrOrchestrationNotFound
		}
		return nil, fmt.Errorf("cortex: get orchestration: %w", err)
	}
	return orchestrationConfigFromModel(m)
}

func (s *Store) GetOrchestrationByName(ctx context.Context, appID, name string) (*orchestration.OrchestrationConfig, error) {
	m := new(orchestrationConfigModel)
	err := s.pgdb.NewSelect(m).Where("app_id = ?", appID).Where("name = ?", name).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, cortex.ErrOrchestrationNotFound
		}
		return nil, fmt.Errorf("cortex: get orchestration by name: %w", err)
	}
	return orchestrationConfigFromModel(m)
}

func (s *Store) UpdateOrchestration(ctx context.Context, c *orchestration.OrchestrationConfig) error {
	c.UpdatedAt = time.Now().UTC()
	res, err := s.pgdb.NewUpdate(orchestrationConfigToModel(c)).WherePK().Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: update orchestration: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex: update orchestration rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrOrchestrationNotFound
	}
	return nil
}

func (s *Store) DeleteOrchestration(ctx context.Context, orchID id.OrchestrationConfigID) error {
	res, err := s.pgdb.NewDelete((*orchestrationConfigModel)(nil)).Where("id = ?", orchID.String()).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: delete orchestration: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex: delete orchestration rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrOrchestrationNotFound
	}
	return nil
}

func (s *Store) ListOrchestrations(ctx context.Context, filter *orchestration.ConfigListFilter) ([]*orchestration.OrchestrationConfig, error) {
	var models []orchestrationConfigModel
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
		return nil, fmt.Errorf("cortex: list orchestrations: %w", err)
	}
	result := make([]*orchestration.OrchestrationConfig, len(models))
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
	q := s.pgdb.NewSelect((*orchestrationConfigModel)(nil))
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
		return 0, fmt.Errorf("cortex: count orchestrations: %w", err)
	}
	return count, nil
}

func (s *Store) CreateOrchestrationRun(ctx context.Context, r *orchestration.OrchestrationRun) error {
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	if _, err := s.pgdb.NewInsert(orchestrationRunToModel(r)).Exec(ctx); err != nil {
		return fmt.Errorf("cortex: create orchestration run: %w", err)
	}
	return nil
}

func (s *Store) GetOrchestrationRun(ctx context.Context, runID id.OrchestrationID) (*orchestration.OrchestrationRun, error) {
	m := new(orchestrationRunModel)
	if err := s.pgdb.NewSelect(m).Where("id = ?", runID.String()).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, cortex.ErrOrchestrationRunNotFound
		}
		return nil, fmt.Errorf("cortex: get orchestration run: %w", err)
	}
	return orchestrationRunFromModel(m)
}

func (s *Store) UpdateOrchestrationRun(ctx context.Context, r *orchestration.OrchestrationRun) error {
	r.UpdatedAt = time.Now().UTC()
	res, err := s.pgdb.NewUpdate(orchestrationRunToModel(r)).WherePK().Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: update orchestration run: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex: update orchestration run rows affected: %w", rowsErr)
	}
	if n == 0 {
		return cortex.ErrOrchestrationRunNotFound
	}
	return nil
}

func (s *Store) ListOrchestrationRuns(ctx context.Context, filter *orchestration.RunListFilter) ([]*orchestration.OrchestrationRun, error) {
	var models []orchestrationRunModel
	q := s.pgdb.NewSelect(&models).OrderExpr("created_at DESC")
	if filter != nil {
		if filter.AppID != "" {
			q = q.Where("app_id = ?", filter.AppID)
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
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list orchestration runs: %w", err)
	}
	result := make([]*orchestration.OrchestrationRun, len(models))
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
	q := s.pgdb.NewSelect((*orchestrationRunModel)(nil))
	if filter != nil {
		if filter.AppID != "" {
			q = q.Where("app_id = ?", filter.AppID)
		}
		if filter.Status != "" {
			q = q.Where("status = ?", filter.Status)
		}
	}
	count, err := q.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex: count orchestration runs: %w", err)
	}
	return count, nil
}
