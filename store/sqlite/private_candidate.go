package sqlite

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/internal/runstate"
)

func (d assuranceDriver) SetRunCandidate(ctx context.Context, r *run.Run, at time.Time) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	revision, err := runstate.NextRevision(r.Revision)
	if err != nil {
		return err
	}
	m := runToModel(r)
	m.Revision, m.UpdatedAt = revision, at
	columns := []string{"revision", "state", "stop_reason", "finish_reason", "output", "verification_status", "result_ref", "step_count", "tokens_used", "usage", "completed_at", "updated_at"}
	if r.State != run.StateRunning {
		m.LeaseOwner, m.LeaseExpiresAt = "", nil
		columns = append(columns, leaseRunColumns...)
	}
	q := d.queries().NewUpdate(m).Column(columns...).WherePK().Where("revision = ?", r.Revision).Where("scope_canon = ?", scope.Canonical())
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	result, err := q.Exec(ctx)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return cortex.ErrRunConflict
	}
	return nil
}
