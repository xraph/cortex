package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

func (s *Store) CreateRun(ctx context.Context, r *run.Run) error {
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	m := runToModel(r)
	_, err := s.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: create run: %w", err)
	}
	return nil
}

func (s *Store) GetRun(ctx context.Context, runID id.AgentRunID) (*run.Run, error) {
	m := new(runModel)
	err := s.db.NewSelect().Model(m).Where("id = ?", runID.String()).Scan(ctx)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, cortex.ErrRunNotFound
		}
		return nil, fmt.Errorf("cortex: get run: %w", err)
	}
	return runFromModel(m), nil
}

func (s *Store) UpdateRun(ctx context.Context, r *run.Run) error {
	r.UpdatedAt = time.Now().UTC()
	m := runToModel(r)
	res, err := s.db.NewUpdate().Model(m).WherePK().Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: update run: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return cortex.ErrRunNotFound
	}
	return nil
}

func (s *Store) ListRuns(ctx context.Context, filter *run.ListFilter) ([]*run.Run, error) {
	var models []runModel
	q := s.db.NewSelect().Model(&models).OrderExpr("created_at DESC")
	if filter != nil {
		if filter.AgentID != "" {
			q = q.Where("agent_id = ?", filter.AgentID)
		}
		if filter.TenantID != "" {
			q = q.Where("tenant_id = ?", filter.TenantID)
		}
		if filter.State != "" {
			q = q.Where("state = ?", string(filter.State))
		}
		if filter.Limit > 0 {
			q = q.Limit(filter.Limit)
		}
		if filter.Offset > 0 {
			q = q.Offset(filter.Offset)
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list runs: %w", err)
	}
	result := make([]*run.Run, len(models))
	for i := range models {
		result[i] = runFromModel(&models[i])
	}
	return result, nil
}

func (s *Store) CreateStep(ctx context.Context, step *run.Step) error {
	now := time.Now().UTC()
	step.CreatedAt = now
	step.UpdatedAt = now
	m := stepToModel(step)
	_, err := s.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: create step: %w", err)
	}
	return nil
}

func (s *Store) ListSteps(ctx context.Context, runID id.AgentRunID) ([]*run.Step, error) {
	var models []stepModel
	err := s.db.NewSelect().Model(&models).
		Where("run_id = ?", runID.String()).
		OrderExpr(`"index" ASC`).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("cortex: list steps: %w", err)
	}
	result := make([]*run.Step, len(models))
	for i := range models {
		result[i] = stepFromModel(&models[i])
	}
	return result, nil
}

func (s *Store) CreateToolCall(ctx context.Context, tc *run.ToolCall) error {
	now := time.Now().UTC()
	tc.CreatedAt = now
	tc.UpdatedAt = now
	m := toolCallToModel(tc)
	_, err := s.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: create tool call: %w", err)
	}
	return nil
}

func (s *Store) ListToolCalls(ctx context.Context, stepID id.StepID) ([]*run.ToolCall, error) {
	var models []toolCallModel
	err := s.db.NewSelect().Model(&models).
		Where("step_id = ?", stepID.String()).
		OrderExpr("created_at ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("cortex: list tool calls: %w", err)
	}
	result := make([]*run.ToolCall, len(models))
	for i := range models {
		result[i] = toolCallFromModel(&models[i])
	}
	return result, nil
}
