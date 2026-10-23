package postgres

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

func (d assuranceDriver) LockSubmissionRun(ctx context.Context, identifier id.AgentRunID) (*run.Run, error) {
	if d.transaction == nil {
		return nil, coordination.ErrAssignmentUnavailable
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	q := d.queries().NewUpdate((*runModel)(nil)).Set("revision = revision").Where("id = ?", identifier.String()).Where("scope_canon = ?", scope.Canonical())
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	result, err := q.Exec(ctx)
	if err != nil {
		return nil, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, cortex.ErrRunNotFound
	}
	return d.GetRun(ctx, identifier)
}
