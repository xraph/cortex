package mongo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/internal/runstate"
)

// CreateRun persists a new run.
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

	query := d.store.mdb.NewInsert(m)
	var err error
	if d.resultTransaction {
		// Keep Grove's stored field mapping without running mutation hooks inside
		// a replayable transaction.
		var document bson.M
		document, err = query.BuildDoc()
		if err == nil {
			_, err = d.store.mdb.Collection(colRuns).InsertOne(ctx, document)
		}
	} else {
		_, err = query.Exec(ctx)
	}
	if err != nil {
		return fmt.Errorf("cortex/mongo: create run: %w", err)
	}

	r.Revision = m.Revision
	return nil
}

// GetRun returns a run by ID within the caller's scope.
func (s *Store) GetRun(ctx context.Context, runID id.AgentRunID) (*run.Run, error) {
	scope, scopeErr := cortex.RequireScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	filter := bson.M{"_id": runID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	var m runModel

	err := s.mdb.NewFind(&m).
		Filter(filter).
		Scan(ctx)
	if err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrRunNotFound
		}

		return nil, fmt.Errorf("cortex/mongo: get run: %w", err)
	}

	return runFromModel(&m)
}

// UpdateRun modifies an existing run's mutable fields within the caller's
// scope. Scope is immutable after creation: the context scope is used
// only as an authorization predicate (the caller must be at the exact scope of the
// run's stored scope to touch it), and is never written back. grove's
// NewUpdate(model).Exec builds $set from every field on the model by
// default (confirmed by reading mongodriver's query_update.go), so this
// builds an explicit $set instead of passing the model through — leaving
// out scope_l0/l1/l2/extra/canon rather than relying on them happening to
// already match. principal is left out too: it is written once, when the
// run is created, and a resume decodes it to find the run's caller.
func (s *Store) UpdateRun(ctx context.Context, r *run.Run) error {
	return (assuranceDriver{store: s}).UpdateRunSnapshot(ctx, r, now())
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

	filter := bson.M{"_id": m.ID, "revision": r.Revision}
	if r.Revision == 0 {
		filter["$or"] = bson.A{bson.M{"revision": int64(0)}, bson.M{"revision": bson.M{"$exists": false}}}
		delete(filter, "revision")
	}
	for k, v := range scopeFilter(scope, true) {
		filter[k] = v
	}

	set := bson.M{
		"revision":   m.Revision,
		"agent_id":   m.AgentID,
		"session_id": m.SessionID,
		"state":      m.State,
		"provenance": m.Provenance, "usage": m.Usage, "budget": m.Budget,
		"budget_deadline_exact": m.BudgetDeadlineExact,

		"stop_reason":         m.StopReason,
		"finish_reason":       m.FinishReason,
		"verification_status": m.VerificationStatus,
		"result_ref":          m.ResultRef,
		"input":               m.Input,
		"output":              m.Output,
		"error":               m.Error,
		"step_count":          m.StepCount,
		"tokens_used":         m.TokensUsed,
		"started_at":          m.StartedAt,
		"completed_at":        m.CompletedAt,
		"persona_ref":         m.PersonaRef,
		"metadata":            m.Metadata,
		"updated_at":          m.UpdatedAt,
	}

	res, err := d.store.mdb.NewUpdate((*runModel)(nil)).
		Filter(filter).
		SetUpdate(bson.M{"$set": set}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update run: %w", err)
	}

	if res.MatchedCount() == 0 {
		return runstate.UpdateConflict(ctx, d, scope, r.ID)
	}

	r.Revision = m.Revision
	r.UpdatedAt = m.UpdatedAt
	return nil
}

// ListRuns returns runs within the caller's scope, optionally filtered.
func (s *Store) ListRuns(ctx context.Context, filter *run.ListFilter) ([]*run.Run, error) {
	scope, scopeErr := cortex.RequireScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	if filter == nil {
		filter = &run.ListFilter{}
	}

	var models []runModel

	f := bson.M{}
	for k, v := range scopeFilter(scope, filter.Exact) {
		f[k] = v
	}
	if filter.AgentID != "" {
		f["agent_id"] = filter.AgentID
	}
	if filter.State != "" {
		if filter.State == run.StatePaused {
			f["state"] = bson.M{"$in": []string{string(run.StatePaused), runstate.PausedPrepared}}
		} else {
			f["state"] = string(filter.State)
		}
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

// CountRuns returns the total number of runs matching the filter within the
// caller's scope.
func (s *Store) CountRuns(ctx context.Context, filter *run.ListFilter) (int64, error) {
	scope, scopeErr := cortex.RequireScope(ctx)
	if scopeErr != nil {
		return 0, scopeErr
	}
	if filter == nil {
		filter = &run.ListFilter{}
	}

	f := bson.M{}
	for k, v := range scopeFilter(scope, filter.Exact) {
		f[k] = v
	}
	if filter.AgentID != "" {
		f["agent_id"] = filter.AgentID
	}
	if filter.State != "" {
		if filter.State == run.StatePaused {
			f["state"] = bson.M{"$in": []string{string(run.StatePaused), runstate.PausedPrepared}}
		} else {
			f["state"] = string(filter.State)
		}
	}

	count, err := s.mdb.NewFind((*runModel)(nil)).
		Filter(f).
		Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("cortex/mongo: count runs: %w", err)
	}

	return count, nil
}

// CreateStep persists a new step.
func (s *Store) CreateStep(ctx context.Context, step *run.Step) error {
	scope, scopeErr := cortex.RequireScope(ctx)
	if scopeErr != nil {
		return scopeErr
	}
	t := now()
	step.CreatedAt = t
	step.UpdatedAt = t
	step.Scope = scope
	m := stepToModel(step)

	_, err := s.mdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: create step: %w", err)
	}

	return nil
}

// ListSteps returns all steps for a run, ordered by index ascending, within
// the caller's scope.
func (s *Store) ListSteps(ctx context.Context, runID id.AgentRunID) ([]*run.Step, error) {
	scope, scopeErr := cortex.RequireScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	filter := bson.M{"run_id": runID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	var models []stepModel

	err := s.mdb.NewFind(&models).
		Filter(filter).
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
	scope, scopeErr := cortex.RequireScope(ctx)
	if scopeErr != nil {
		return scopeErr
	}
	t := now()
	tc.CreatedAt = t
	tc.UpdatedAt = t
	tc.Scope = scope
	m := toolCallToModel(tc)

	_, err := s.mdb.NewInsert(m).Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: create tool call: %w", err)
	}

	return nil
}

// ListToolCalls returns all tool calls for a step, ordered by creation
// time, within the caller's scope.
func (s *Store) ListToolCalls(ctx context.Context, stepID id.StepID) ([]*run.ToolCall, error) {
	scope, scopeErr := cortex.RequireScope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}

	filter := bson.M{"step_id": stepID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	var models []toolCallModel

	err := s.mdb.NewFind(&models).
		Filter(filter).
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
