package sqlite

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/suspension"
)

func (d assuranceDriver) LockSuspension(ctx context.Context, runID id.AgentRunID) (*suspension.Suspension, error) {
	if d.transaction == nil {
		return nil, suspension.ErrVersionClaimUnavailable
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	lock := d.queries().NewUpdate((*suspensionModel)(nil)).Set("updated_at = updated_at").Where("run_id = ?", runID.String()).Where("scope_canon = ?", scope.Canonical())
	for _, p := range scopePredicates(scope, true) {
		lock = lock.Where(p.Column+" = ?", p.Value)
	}
	changed, err := lock.Exec(ctx)
	if err != nil {
		return nil, err
	}
	count, err := changed.RowsAffected()
	if err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, cortex.ErrNotSuspended
	}
	model := new(suspensionModel)
	q := d.queries().NewSelect(model).Where("run_id = ?", runID.String()).Where("scope_canon = ?", scope.Canonical())
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err = q.Scan(ctx); err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrNotSuspended
		}
		return nil, err
	}
	return suspensionFromModel(model)
}
