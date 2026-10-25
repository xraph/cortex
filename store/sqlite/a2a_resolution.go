package sqlite

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/store/internal/assurance"
)

func (d assuranceDriver) TransactAskResolution(ctx context.Context, token string, callback func(context.Context, assurance.AskResolutionTransaction) error) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	tx, err := d.store.sdb.BeginTxQuery(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // Rollback after commit is harmless.
	transactional := assuranceDriver{store: d.store, transaction: tx}
	q := tx.NewUpdate((*a2aPendingAskModel)(nil)).Set("updated_at = updated_at").Where("reply_with = ?", token).Where("scope_canon = ?", scope.Canonical()).Where("orchestration_id = ?", a2a.RoutingOrchestration(ctx))
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	changed, err := q.Exec(ctx)
	if err != nil {
		return err
	}
	count, err := changed.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return a2a.ErrAskNotFound
	}
	if callbackErr := callback(ctx, transactional); callbackErr != nil {
		return callbackErr
	}
	return tx.Commit()
}

func (d assuranceDriver) ReadPendingAsk(ctx context.Context, token string) (*a2a.PendingAsk, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	model := new(a2aPendingAskModel)
	q := d.queries().NewSelect(model).Where("reply_with = ?", token).Where("scope_canon = ?", scope.Canonical()).Where("orchestration_id = ?", a2a.RoutingOrchestration(ctx))
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err = q.Scan(ctx); err != nil {
		if isNoRows(err) {
			return nil, a2a.ErrAskNotFound
		}
		return nil, err
	}
	return a2aPendingAskFromModel(model)
}

func (d assuranceDriver) ClaimResolvedAsk(ctx context.Context, token string, at time.Time) error {
	if d.transaction == nil {
		return a2a.ErrAskResolutionUnavailable
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	q := d.queries().NewUpdate((*a2aPendingAskModel)(nil)).Set("claimed_at = ?", at).Set("updated_at = ?", at).Where("reply_with = ?", token).Where("scope_canon = ?", scope.Canonical()).Where("orchestration_id = ?", a2a.RoutingOrchestration(ctx)).Where("claimed_at IS NULL")
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	changed, err := q.Exec(ctx)
	if err != nil {
		return err
	}
	count, err := changed.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return a2a.ErrAskAlreadyClaimed
	}
	return nil
}
