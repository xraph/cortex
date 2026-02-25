package mongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

// CreateRun persists a new run.
func (s *Store) CreateRun(ctx context.Context, r *run.Run) error {
	t := now()
	r.CreatedAt = t
	r.UpdatedAt = t
	m := runToModel(r)

	_, err := s.mdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: create run: %w", err)
	}

	return nil
}

// GetRun returns a run by ID.
func (s *Store) GetRun(ctx context.Context, runID id.AgentRunID) (*run.Run, error) {
	var m runModel

	err := s.mdb.NewFind(&m).
		Filter(bson.M{"_id": runID.String()}).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrRunNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get run: %w", err)
	}

	return runFromModel(&m)
}

// UpdateRun modifies an existing run.
func (s *Store) UpdateRun(ctx context.Context, r *run.Run) error {
	r.UpdatedAt = now()
	m := runToModel(r)

	res, err := s.mdb.NewUpdate(m).
		Filter(bson.M{"_id": m.ID}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update run: %w", err)
	}

	if res.MatchedCount() == 0 {
		return cortex.ErrRunNotFound
	}

	return nil
}

// ListRuns returns runs, optionally filtered.
func (s *Store) ListRuns(ctx context.Context, filter *run.ListFilter) ([]*run.Run, error) {
	var models []runModel

	f := bson.M{}
	if filter != nil {
		if filter.AgentID != "" {
			f["agent_id"] = filter.AgentID
		}

		if filter.TenantID != "" {
			f["tenant_id"] = filter.TenantID
		}

		if filter.State != "" {
			f["state"] = string(filter.State)
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
		return nil, fmt.Errorf("cortex/mongo: list runs: %w", err)
	}

	result := make([]*run.Run, len(models))
	for i := range models {
		r, convErr := runFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = r
	}

	return result, nil
}

// CreateStep persists a new step.
func (s *Store) CreateStep(ctx context.Context, step *run.Step) error {
	t := now()
	step.CreatedAt = t
	step.UpdatedAt = t
	m := stepToModel(step)

	_, err := s.mdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: create step: %w", err)
	}

	return nil
}

// ListSteps returns all steps for a run, ordered by index ascending.
func (s *Store) ListSteps(ctx context.Context, runID id.AgentRunID) ([]*run.Step, error) {
	var models []stepModel

	err := s.mdb.NewFind(&models).
		Filter(bson.M{"run_id": runID.String()}).
		Sort(bson.D{{Key: "index", Value: 1}}).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("cortex/mongo: list steps: %w", err)
	}

	result := make([]*run.Step, len(models))
	for i := range models {
		st, convErr := stepFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = st
	}

	return result, nil
}

// CreateToolCall persists a new tool call.
func (s *Store) CreateToolCall(ctx context.Context, tc *run.ToolCall) error {
	t := now()
	tc.CreatedAt = t
	tc.UpdatedAt = t
	m := toolCallToModel(tc)

	_, err := s.mdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: create tool call: %w", err)
	}

	return nil
}

// ListToolCalls returns all tool calls for a step, ordered by creation time.
func (s *Store) ListToolCalls(ctx context.Context, stepID id.StepID) ([]*run.ToolCall, error) {
	var models []toolCallModel

	err := s.mdb.NewFind(&models).
		Filter(bson.M{"step_id": stepID.String()}).
		Sort(bson.D{{Key: "created_at", Value: 1}}).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("cortex/mongo: list tool calls: %w", err)
	}

	result := make([]*run.ToolCall, len(models))
	for i := range models {
		tc, convErr := toolCallFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		result[i] = tc
	}

	return result, nil
}
