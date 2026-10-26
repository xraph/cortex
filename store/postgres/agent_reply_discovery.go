package postgres

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store/internal/replydiscovery"
	"github.com/xraph/cortex/suspension"
)

// ListAgentReplyRuns projects bounded run identities from exact-scope suspensions.
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
	return replydiscovery.Parse(rows, after, limit)
}
