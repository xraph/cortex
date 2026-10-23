package sqlite

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

func (d assuranceDriver) TransactRun(ctx context.Context, runID id.AgentRunID, callback func(context.Context, assurance.RunTransaction) error) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	tx, err := d.store.sdb.BeginTxQuery(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // Rollback after commit is harmless.
	// Acquire the write lock before any read. SQLite must not upgrade a deferred
	// read transaction; PostgreSQL serializes writers of this exact run row.
	q := tx.NewUpdate((*runModel)(nil)).Set("revision = revision").Where("id = ?", runID.String()).Where("scope_canon = ?", scope.Canonical())
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if _, err = q.Exec(ctx); err != nil {
		return err
	}
	if callbackErr := callback(ctx, assuranceDriver{store: d.store, transaction: tx}); callbackErr != nil {
		return callbackErr
	}
	return tx.Commit()
}

func (d assuranceDriver) GetRun(ctx context.Context, runID id.AgentRunID) (*run.Run, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	m := new(runModel)
	q := d.queries().NewSelect(m).Where("id = ?", runID.String()).Where("scope_canon = ?", scope.Canonical())
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err = q.Scan(ctx); err != nil {
		if isNoRows(err) {
			return nil, cortex.ErrRunNotFound
		}
		return nil, err
	}
	return runFromModel(m)
}

func (d assuranceDriver) SetRunVerification(ctx context.Context, runID id.AgentRunID, revision int64, status verification.Status, resultID id.ID, at time.Time) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	q := d.queries().NewUpdate((*runModel)(nil)).Set("revision = ?", revision+1).Set("verification_status = ?", string(status)).Set("result_ref = ?", resultID.String()).Set("updated_at = ?", at).Where("id = ?", runID.String()).Where("revision = ?", revision).Where("scope_canon = ?", scope.Canonical())
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	result, err := q.Exec(ctx)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return cortex.ErrRunConflict
	}
	return nil
}

func (d assuranceDriver) ReplaceJob(ctx context.Context, row assurance.Record, expectedBody string) error {
	if row.Kind != assurance.KindJob {
		return verification.ErrInvalid
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	q := d.queries().NewUpdate((*assurance.Record)(nil)).Set("body = ?", row.Body).Set("digest = ?", row.Digest).Where("kind = ?", assurance.KindJob).Where("id = ?", row.ID).Where("scope_canon = ?", scope.Canonical()).Where("body = ?", expectedBody).Where("deleted = ?", false)
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	result, err := q.Exec(ctx)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return verification.ErrJobConflict
	}
	return nil
}
