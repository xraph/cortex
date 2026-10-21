package sqlstore

import (
	"context"
	"fmt"

	"github.com/xraph/cortex/optimization"
)

func (b *backend) Migrate(ctx context.Context) error {
	tx, err := b.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // Commit or the triggering error determines the result.
	if b.sql.kind == PostgreSQL {
		// PostgreSQL can race while creating catalog entries even with IF NOT
		// EXISTS. This transaction-scoped lock serializes schema initialization.
		if _, lockErr := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('cortex.optimization.schema', 0))`); lockErr != nil {
			return lockErr
		}
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS cortex_optimization_schema (singleton INTEGER PRIMARY KEY CHECK(singleton = 1), version INTEGER NOT NULL)`,
		`INSERT INTO cortex_optimization_schema (singleton, version) VALUES (1, 1) ON CONFLICT(singleton) DO NOTHING`,
	} {
		if _, err = tx.Exec(ctx, statement); err != nil {
			return err
		}
	}
	var version int
	if versionErr := tx.QueryRow(ctx, `SELECT version FROM cortex_optimization_schema WHERE singleton = 1`).Scan(&version); versionErr != nil {
		return versionErr
	}
	if version != 1 {
		return fmt.Errorf("%w: unsupported optimization schema version %d", optimization.ErrUnavailable, version)
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS cortex_optimization_scopes (scope_hash TEXT PRIMARY KEY, scope_canon TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS cortex_optimization_records (
		 scope_hash TEXT NOT NULL, scope_canon TEXT NOT NULL,
		 scope_l0 TEXT NOT NULL, scope_l1 TEXT NOT NULL, scope_l2 TEXT NOT NULL,
		 record_id TEXT NOT NULL, schema_version INTEGER NOT NULL,
		 kind TEXT NOT NULL, parent_key TEXT NOT NULL, item_key TEXT NOT NULL,
		 digest TEXT NOT NULL, body BLOB NOT NULL,
		 PRIMARY KEY(scope_hash, record_id))`,
		`CREATE INDEX IF NOT EXISTS cortex_optimization_record_kind ON cortex_optimization_records(scope_hash, kind, parent_key)`,
	} {
		if _, err = tx.Exec(ctx, statement); err != nil {
			return err
		}
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	return tx.Commit()
}
