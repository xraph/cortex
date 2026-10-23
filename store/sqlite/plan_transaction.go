package sqlite

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/store/internal/assurance"
)

func (d assuranceDriver) TransactPlan(ctx context.Context, planID id.ID, callback func(context.Context, assurance.Driver) error) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	tx, err := d.store.sdb.BeginTxQuery(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // Rollback after commit is harmless.
	// Lock before reading so plan revisions and assignment observations serialize.
	q := tx.NewUpdate((*assurance.Record)(nil)).Set("digest = digest").Where("kind = ?", assurance.KindPlan).Where("id = ?", planID.String()).Where("scope_canon = ?", scope.Canonical()).Where("deleted = ?", false)
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
		return planning.ErrNotFound
	}
	if callbackErr := callback(ctx, assuranceDriver{store: d.store, transaction: tx}); callbackErr != nil {
		return callbackErr
	}
	return tx.Commit()
}
