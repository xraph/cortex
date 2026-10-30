package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/internal/runstate"
)

// mutableRunColumns is every cortex_runs column UpdateRun is allowed to
// write. A run's scope is set once at creation and never rewritten:
// scope_l0/l1/l2/extra/canon are deliberately absent here. Grove's
// NewUpdate builds SET from every model field by default, so without this
// explicit whitelist an UpdateRun issued from a broader (but still
// scope-matching) context would silently overwrite a row's narrower
// stored scope. principal is absent for the same reason: the codec bytes
// are written when the run is created, and a resume decodes them to find
// the run's caller, so no later writer may replace them. reply_route is
// absent too: the engine writes a directive's reply route when it creates
// the run, and nothing rewrites it. The lease,
// readiness, reclaim count and notification time are absent too: they
// belong to run.ContinuationStore.
var mutableRunColumns = []string{
	"revision",
	"agent_id",
	"session_id",
	"state",
	"provenance", "usage", "budget", "stop_reason", "finish_reason", "verification_status", "result_ref",
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

// leaseRunColumns hold a running run's lease. UpdateRun writes them only
// to clear them, when the run leaves running; HoldLease and the
// continuation transitions are the only other writers.
var leaseRunColumns = []string{"lease_owner", "lease_expires_at"}

func (s *Store) CreateRun(ctx context.Context, r *run.Run) error {
	return (assuranceDriver{store: s}).insertRun(ctx, r, time.Now().UTC())
}

func (d assuranceDriver) insertRun(ctx context.Context, r *run.Run, at time.Time) error {
	scope, scopeErr := cortex.RequireScope(ctx)
	if scopeErr != nil {
		return scopeErr
	}
	r.CreatedAt = at
	r.UpdatedAt = at
	scope.Levels = append([]cortex.Level(nil), scope.Levels...)
	r.Scope = scope
	m := runToModel(r)
	m.Revision = 1
	_, err := d.queries().NewInsert(m).Exec(ctx)
	if isUniqueViolation(err) {
		return fmt.Errorf("cortex/sqlite: create run %s: %w: %w", r.ID, cortex.ErrAlreadyExists, err)
	}
	if err != nil {
		return fmt.Errorf("cortex/sqlite: create run: %w", err)
	}
	r.Revision = m.Revision
	return nil
}

func (s *Store) GetRun(ctx context.Context, runID id.AgentRunID) (*run.Run, error) {
	scope, scopeErr := cortex.RequireScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	m := new(runModel)
	q := s.sdb.NewSelect(m).Where("id = ?", runID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	err := q.Scan(ctx)
	if err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrRunNotFound
		}
		return nil, fmt.Errorf("cortex/sqlite: get run: %w", err)
	}
	return runFromModel(m)
}

// UpdateRun updates a run's mutable fields. Scope is immutable after
// creation: the context scope is used only as an authorization predicate
// (the caller must be at the exact scope of the run's stored scope to touch it), and
// is never written back. r.Scope is left untouched — whatever runToModel
// derives from it is excluded from the SET clause by mutableRunColumns.
func (s *Store) UpdateRun(ctx context.Context, r *run.Run) error {
	return (assuranceDriver{store: s}).UpdateRunSnapshot(ctx, r, time.Now().UTC())
}

func (d assuranceDriver) UpdateRunSnapshot(ctx context.Context, r *run.Run, at time.Time) error {
	scope, scopeErr := cortex.RequireScope(ctx)
	if scopeErr != nil {
		return scopeErr
	}
	next, revisionErr := runstate.NextRevision(r.Revision)
	if revisionErr != nil {
		return revisionErr
	}
	m := runToModel(r)
	m.Revision = next
	m.UpdatedAt = at
	columns := mutableRunColumns
	if r.State != run.StateRunning {
		// Only a running run is held. Leaving running releases the lease,
		// and nothing else UpdateRun does may touch it: a heartbeat moves
		// the expiry without advancing the revision, so a step write from
		// the same worker would otherwise wind it back.
		m.LeaseOwner, m.LeaseExpiresAt = "", nil
		columns = append(append([]string(nil), mutableRunColumns...), leaseRunColumns...)
	}
	q := d.queries().NewUpdate(m).Column(columns...).WherePK().Where("revision = ?", r.Revision)
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: update run: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex/sqlite: update run rows affected: %w", rowsErr)
	}
	if n == 0 {
		return runstate.UpdateConflict(ctx, d, scope, r.ID)
	}
	r.Revision = m.Revision
	r.UpdatedAt = m.UpdatedAt
	if r.State != run.StateRunning {
		r.Lease = nil
	}
	return nil
}

func (s *Store) ListRuns(ctx context.Context, filter *run.ListFilter) ([]*run.Run, error) {
	scope, scopeErr := cortex.RequireScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	if filter == nil {
		filter = &run.ListFilter{}
	}

	var models []runModel
	q := s.sdb.NewSelect(&models).OrderExpr("created_at DESC")
	for _, p := range scopePredicates(scope, filter.Exact) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if filter.AgentID != "" {
		q = q.Where("agent_id = ?", filter.AgentID)
	}
	if filter.State != "" {
		if filter.State == run.StatePaused {
			q = q.Where("state IN (?, ?)", string(run.StatePaused), runstate.PausedPrepared)
		} else {
			q = q.Where("state = ?", string(filter.State))
		}
	}
	if filter.Limit > 0 {
		q = q.Limit(filter.Limit)
	}
	if filter.Offset > 0 {
		q = q.Offset(filter.Offset)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/sqlite: list runs: %w", err)
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

func (s *Store) CountRuns(ctx context.Context, filter *run.ListFilter) (int64, error) {
	scope, scopeErr := cortex.RequireScope(ctx)
	if scopeErr != nil {
		return 0, scopeErr
	}
	if filter == nil {
		filter = &run.ListFilter{}
	}

	q := s.sdb.NewSelect((*runModel)(nil))
	for _, p := range scopePredicates(scope, filter.Exact) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if filter.AgentID != "" {
		q = q.Where("agent_id = ?", filter.AgentID)
	}
	if filter.State != "" {
		if filter.State == run.StatePaused {
			q = q.Where("state IN (?, ?)", string(run.StatePaused), runstate.PausedPrepared)
		} else {
			q = q.Where("state = ?", string(filter.State))
		}
	}
	count, err := q.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/sqlite: count runs: %w", err)
	}
	return count, nil
}

func (s *Store) CreateStep(ctx context.Context, step *run.Step) error {
	scope, scopeErr := cortex.RequireScope(ctx)
	if scopeErr != nil {
		return scopeErr
	}
	now := time.Now().UTC()
	step.CreatedAt = now
	step.UpdatedAt = now
	step.Scope = scope
	m := stepToModel(step)
	_, err := s.sdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: create step: %w", err)
	}
	return nil
}

func (s *Store) ListSteps(ctx context.Context, runID id.AgentRunID) ([]*run.Step, error) {
	scope, scopeErr := cortex.RequireScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	var models []stepModel
	q := s.sdb.NewSelect(&models).
		Where("run_id = ?", runID.String()).
		OrderExpr("\"index\" ASC")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/sqlite: list steps: %w", err)
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

func (s *Store) CreateToolCall(ctx context.Context, tc *run.ToolCall) error {
	scope, scopeErr := cortex.RequireScope(ctx)
	if scopeErr != nil {
		return scopeErr
	}
	now := time.Now().UTC()
	tc.CreatedAt = now
	tc.UpdatedAt = now
	tc.Scope = scope
	m := toolCallToModel(tc)
	_, err := s.sdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: create tool call: %w", err)
	}
	return nil
}

func (s *Store) ListToolCalls(ctx context.Context, stepID id.StepID) ([]*run.ToolCall, error) {
	scope, scopeErr := cortex.RequireScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	var models []toolCallModel
	q := s.sdb.NewSelect(&models).
		Where("step_id = ?", stepID.String()).
		OrderExpr("created_at ASC")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/sqlite: list tool calls: %w", err)
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
