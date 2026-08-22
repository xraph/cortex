package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

// mutableRunColumns is every cortex_runs column UpdateRun is allowed to
// write. A run's scope is set once at creation and never rewritten:
// scope_l0/l1/l2/extra/canon are deliberately absent here. Grove's
// NewUpdate builds SET from every model field by default, so without this
// explicit whitelist an UpdateRun issued from a broader (but still
// scope-matching) context would silently overwrite a row's narrower
// stored scope.
var mutableRunColumns = []string{
	"agent_id",
	"state",
	"input",
	"output",
	"error",
	"step_count",
	"tokens_used",
	"started_at",
	"completed_at",
	"persona_ref",
	"metadata",
	"created_at",
	"updated_at",
}

func (s *Store) CreateRun(ctx context.Context, r *run.Run) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	r.Scope = scope
	m := runToModel(r)
	_, err := s.pgdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: create run: %w", err)
	}
	return nil
}

func (s *Store) GetRun(ctx context.Context, runID id.AgentRunID) (*run.Run, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(runModel)
	q := s.pgdb.NewSelect(m).Where("id = ?", runID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	err := q.Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, cortex.ErrRunNotFound
		}
		return nil, fmt.Errorf("cortex: get run: %w", err)
	}
	return runFromModel(m)
}

// UpdateRun updates a run's mutable fields. Scope is immutable after
// creation: the context scope is used only as an authorization predicate
// (the caller must be at or above the run's stored scope to touch it), and
// is never written back. r.Scope is left untouched — whatever runToModel
// derives from it is excluded from the SET clause by mutableRunColumns.
func (s *Store) UpdateRun(ctx context.Context, r *run.Run) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	r.UpdatedAt = time.Now().UTC()
	m := runToModel(r)
	q := s.pgdb.NewUpdate(m).Column(mutableRunColumns...).WherePK()
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: update run: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cortex: update run rows affected: %w", err)
	}
	if n == 0 {
		return cortex.ErrRunNotFound
	}
	return nil
}

func (s *Store) ListRuns(ctx context.Context, filter *run.ListFilter) ([]*run.Run, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &run.ListFilter{}
	}

	var models []runModel
	q := s.pgdb.NewSelect(&models).OrderExpr("created_at DESC")
	for _, p := range scopePredicates(scope, filter.Exact) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if filter.AgentID != "" {
		q = q.Where("agent_id = ?", filter.AgentID)
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
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list runs: %w", err)
	}
	result := make([]*run.Run, len(models))
	for i := range models {
		r, err := runFromModel(&models[i])
		if err != nil {
			return nil, fmt.Errorf("cortex: list runs: %w", err)
		}
		result[i] = r
	}
	return result, nil
}

func (s *Store) CountRuns(ctx context.Context, filter *run.ListFilter) (int64, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return 0, cortex.ErrNoScope
	}
	if filter == nil {
		filter = &run.ListFilter{}
	}

	q := s.pgdb.NewSelect((*runModel)(nil))
	for _, p := range scopePredicates(scope, filter.Exact) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if filter.AgentID != "" {
		q = q.Where("agent_id = ?", filter.AgentID)
	}
	if filter.State != "" {
		q = q.Where("state = ?", string(filter.State))
	}
	count, err := q.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex: count runs: %w", err)
	}
	return count, nil
}

func (s *Store) CreateStep(ctx context.Context, step *run.Step) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	now := time.Now().UTC()
	step.CreatedAt = now
	step.UpdatedAt = now
	step.Scope = scope
	m := stepToModel(step)
	_, err := s.pgdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: create step: %w", err)
	}
	return nil
}

func (s *Store) ListSteps(ctx context.Context, runID id.AgentRunID) ([]*run.Step, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var models []stepModel
	q := s.pgdb.NewSelect(&models).
		Where("run_id = ?", runID.String()).
		OrderExpr(`"index" ASC`)
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list steps: %w", err)
	}
	result := make([]*run.Step, len(models))
	for i := range models {
		st, err := stepFromModel(&models[i])
		if err != nil {
			return nil, fmt.Errorf("cortex: list steps: %w", err)
		}
		result[i] = st
	}
	return result, nil
}

func (s *Store) CreateToolCall(ctx context.Context, tc *run.ToolCall) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	now := time.Now().UTC()
	tc.CreatedAt = now
	tc.UpdatedAt = now
	tc.Scope = scope
	m := toolCallToModel(tc)
	_, err := s.pgdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: create tool call: %w", err)
	}
	return nil
}

func (s *Store) ListToolCalls(ctx context.Context, stepID id.StepID) ([]*run.ToolCall, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var models []toolCallModel
	q := s.pgdb.NewSelect(&models).
		Where("step_id = ?", stepID.String()).
		OrderExpr("created_at ASC")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list tool calls: %w", err)
	}
	result := make([]*run.ToolCall, len(models))
	for i := range models {
		tc, err := toolCallFromModel(&models[i])
		if err != nil {
			return nil, fmt.Errorf("cortex: list tool calls: %w", err)
		}
		result[i] = tc
	}
	return result, nil
}
