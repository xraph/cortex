package postgres

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/internal/replydiscovery"
	"github.com/xraph/cortex/suspension"
)

// ListAgentReplyRuns includes saved pauses and running admissions whose pause was removed.
func (s *Store) ListAgentReplyRuns(ctx context.Context, after id.AgentRunID, limit int) ([]id.AgentRunID, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	if validationErr := replydiscovery.Validate(after, limit); validationErr != nil {
		return nil, validationErr
	}
	var models []suspensionModel
	q := s.pgdb.NewSelect(&models).Column("run_id").Where("scope_canon = ?", scope.Canonical()).Where("reason = ?", string(suspension.ReasonAgentReply)).OrderExpr("run_id ASC").Limit(limit)
	for _, predicate := range scopePredicates(scope, true) {
		q = q.Where(predicate.Column+" = ?", predicate.Value)
	}
	if !after.IsNil() {
		q = q.Where("run_id > ?", after.String())
	}
	if scanErr := q.Scan(ctx); scanErr != nil {
		return nil, scanErr
	}
	rows := make([]string, 0, len(models))
	for _, model := range models {
		rows = append(rows, model.RunID)
	}
	var admitted []runModel
	active := s.pgdb.NewSelect(&admitted).Column("id").Where("scope_canon = ?", scope.Canonical()).Where("state = ?", string(run.StateRunning)).Where("id IN (SELECT run_id FROM cortex_assurance_records WHERE kind = ? AND scope_canon = ? AND deleted = ?)", "resume_admission", scope.Canonical(), false).OrderExpr("id ASC").Limit(limit)
	for _, predicate := range scopePredicates(scope, true) {
		active = active.Where(predicate.Column+" = ?", predicate.Value)
	}
	if !after.IsNil() {
		active = active.Where("id > ?", after.String())
	}
	if scanErr := active.Scan(ctx); scanErr != nil {
		return nil, scanErr
	}
	activeRows := make([]string, 0, len(admitted))
	for _, model := range admitted {
		activeRows = append(activeRows, model.ID)
	}
	return replydiscovery.Merge(rows, activeRows, after, limit)
}
