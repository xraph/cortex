package postgres

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/store/internal/assurance"
)

func (d assuranceDriver) TransactPlan(ctx context.Context, planID id.ID, callback func(context.Context, assurance.Driver) error) error {
	return d.transactAssuranceRecord(ctx, assurance.KindPlan, planID, planning.ErrNotFound, callback)
}

func (d assuranceDriver) TransactRunCalls(ctx context.Context, runID id.AgentRunID, callback func(context.Context, assurance.Driver) error) error {
	return d.transactAssuranceRecord(ctx, assurance.KindRunCallGuard, runID, execution.ErrInvalidRunCall, callback)
}

// LockRunCalls joins an existing storage transaction; it never opens another.
func (d assuranceDriver) LockRunCalls(ctx context.Context, runID id.AgentRunID) error {
	if d.transaction == nil {
		return execution.ErrRunCallUnavailable
	}
	if runID.IsNil() || runID.Prefix() != id.PrefixAgentRun {
		return execution.ErrInvalidRunCall
	}
	return d.lockAssuranceRecord(ctx, assurance.KindRunCallGuard, runID, execution.ErrInvalidRunCall)
}

func (d assuranceDriver) transactAssuranceRecord(ctx context.Context, kind string, identifier id.ID, missing error, callback func(context.Context, assurance.Driver) error) error {
	if _, err := cortex.RequireScope(ctx); err != nil {
		return err
	}
	tx, err := d.store.pgdb.BeginTxQuery(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // Rollback after commit is harmless.
	transactional := assuranceDriver{store: d.store, transaction: tx}
	if lockErr := transactional.lockAssuranceRecord(ctx, kind, identifier, missing); lockErr != nil {
		return lockErr
	}
	if callbackErr := callback(ctx, transactional); callbackErr != nil {
		return callbackErr
	}
	return tx.Commit()
}

func (d assuranceDriver) lockAssuranceRecord(ctx context.Context, kind string, identifier id.ID, missing error) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	// Lock before reading so every writer using this exact assurance anchor
	// observes earlier committed changes. The outer transaction owns the lock.
	q := d.transaction.NewUpdate((*assurance.Record)(nil)).Set("digest = digest").Where("kind = ?", kind).Where("id = ?", identifier.String()).Where("scope_canon = ?", scope.Canonical()).Where("deleted = ?", false)
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
		return missing
	}
	return nil
}
