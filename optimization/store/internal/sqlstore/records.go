package sqlstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/xraph/grove/driver"

	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store/internal/scoped"
	"github.com/xraph/cortex/optimization/store/internal/state"
)

type queryer interface {
	Query(context.Context, string, ...any) (driver.Rows, error)
}

func (b *backend) Load(ctx context.Context, key string) ([]state.Record, error) {
	scope, err := scoped.New(ctx, key)
	if err != nil {
		return nil, err
	}
	return load(ctx, b.sql, scope, nil)
}

func (b *backend) LoadRecord(ctx context.Context, key string, record state.RecordKey) ([]state.Record, error) {
	scope, err := scoped.New(ctx, key)
	if err != nil {
		return nil, err
	}
	return load(ctx, b.sql, scope, &record)
}

func load(ctx context.Context, query queryer, scope scoped.Index, record *state.RecordKey) (records []state.Record, resultErr error) {
	statement := `SELECT schema_version, scope_canon, kind, parent_key, item_key, record_id, digest, body
	 FROM cortex_optimization_records WHERE scope_hash = ? AND scope_canon = ? AND scope_l0 = ? AND scope_l1 = ? AND scope_l2 = ?`
	args := []any{scope.Hash, scope.Canon, scope.Levels[0], scope.Levels[1], scope.Levels[2]}
	if record != nil {
		statement += ` AND record_id = ? LIMIT 2`
		args = append(args, record.ID())
	}
	rows, err := query.Query(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, rows.Close()) }()
	for rows.Next() {
		var record state.Record
		if scanErr := rows.Scan(&record.Schema, &record.Scope, &record.Kind, &record.Parent, &record.Key, &record.ID, &record.Digest, &record.Body); scanErr != nil {
			return nil, scanErr
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (b *backend) Update(ctx context.Context, key string, update func([]state.Record) ([]state.Mutation, error)) error {
	scope, err := scoped.New(ctx, key)
	if err != nil {
		return err
	}
	tx, err := b.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // Commit or the triggering error determines the result.
	// The first statement locks the scope row (SQLite locks the database)
	// before reading state. The canonical key also detects hash collisions.
	if _, err = tx.Exec(ctx, `INSERT INTO cortex_optimization_scopes (scope_hash, scope_canon) VALUES (?, ?)
	 ON CONFLICT(scope_hash) DO UPDATE SET scope_canon = cortex_optimization_scopes.scope_canon`, scope.Hash, scope.Canon); err != nil {
		return err
	}
	var storedScope string
	if scopeErr := tx.QueryRow(ctx, `SELECT scope_canon FROM cortex_optimization_scopes WHERE scope_hash = ?`, scope.Hash).Scan(&storedScope); scopeErr != nil {
		return scopeErr
	}
	if storedScope != scope.Canon {
		return fmt.Errorf("%w: stored scope digest collision", optimization.ErrUnavailable)
	}
	records, err := load(ctx, tx, scope, nil)
	if err != nil {
		return err
	}
	changes, err := update(records)
	if err != nil {
		return err
	}
	for _, change := range changes {
		if applyErr := apply(ctx, tx, scope, change); applyErr != nil {
			return applyErr
		}
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	return tx.Commit()
}

func apply(ctx context.Context, tx driver.Tx, scope scoped.Index, change state.Mutation) error {
	record := change.Record
	if record.Scope != scope.Canon {
		return optimization.ErrPolicyDenied
	}
	var result driver.Result
	var err error
	if change.ExpectedDigest == "" {
		result, err = tx.Exec(ctx, `INSERT INTO cortex_optimization_records
		 (scope_hash, scope_canon, scope_l0, scope_l1, scope_l2, record_id, schema_version, kind, parent_key, item_key, digest, body)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(scope_hash, record_id) DO NOTHING`,
			scope.Hash, scope.Canon, scope.Levels[0], scope.Levels[1], scope.Levels[2], record.ID, record.Schema, record.Kind, record.Parent, record.Key, record.Digest, record.Body)
	} else {
		result, err = tx.Exec(ctx, `UPDATE cortex_optimization_records SET schema_version = ?, digest = ?, body = ?
		 WHERE scope_hash = ? AND scope_canon = ? AND scope_l0 = ? AND scope_l1 = ? AND scope_l2 = ?
		 AND record_id = ? AND kind = ? AND parent_key = ? AND item_key = ? AND digest = ?`,
			record.Schema, record.Digest, record.Body, scope.Hash, scope.Canon, scope.Levels[0], scope.Levels[1], scope.Levels[2], record.ID, record.Kind, record.Parent, record.Key, change.ExpectedDigest)
	}
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return optimization.ErrConflict
	}
	return nil
}
