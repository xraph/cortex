package postgres

import (
	"context"
	"fmt"

	"github.com/xraph/grove/driver"
	"github.com/xraph/grove/drivers/pgdriver"
	"github.com/xraph/grove/migrate"
)

const (
	migrationTableName = "grove_migrations"
	lockTableName      = "grove_migration_locks"
)

// pgMigrateExecutor implements migrate.Executor for PostgreSQL using grove's pgdriver.
type pgMigrateExecutor struct {
	pgdb *pgdriver.PgDB
}

var _ migrate.Executor = (*pgMigrateExecutor)(nil)

// Exec executes a SQL statement that does not return rows.
func (e *pgMigrateExecutor) Exec(ctx context.Context, query string, args ...any) (driver.Result, error) {
	return e.pgdb.Exec(ctx, query, args...)
}

// Query executes a SQL statement that returns rows.
func (e *pgMigrateExecutor) Query(ctx context.Context, query string, args ...any) (driver.Rows, error) {
	return e.pgdb.Query(ctx, query, args...)
}

// EnsureMigrationTable creates the grove_migrations table if it does not exist.
func (e *pgMigrateExecutor) EnsureMigrationTable(ctx context.Context) error {
	query := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		id          BIGSERIAL PRIMARY KEY,
		version     VARCHAR(14) NOT NULL,
		name        VARCHAR(255) NOT NULL,
		"group"     VARCHAR(255) NOT NULL,
		migrated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		UNIQUE(version, "group")
	)`, migrationTableName)
	_, err := e.pgdb.Exec(ctx, query)
	return err
}

// EnsureLockTable creates the grove_migration_locks table if it does not exist.
func (e *pgMigrateExecutor) EnsureLockTable(ctx context.Context) error {
	query := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		id          INT PRIMARY KEY DEFAULT 1,
		locked_at   TIMESTAMPTZ,
		locked_by   VARCHAR(255),
		CONSTRAINT single_lock CHECK (id = 1)
	)`, lockTableName)
	_, err := e.pgdb.Exec(ctx, query)
	return err
}

// AcquireLock attempts to acquire the distributed migration lock using
// PostgreSQL advisory locks.
func (e *pgMigrateExecutor) AcquireLock(ctx context.Context, lockedBy string) error {
	row := e.pgdb.QueryRow(ctx, "SELECT pg_try_advisory_lock(1)")
	var acquired bool
	if err := row.Scan(&acquired); err != nil {
		return fmt.Errorf("cortex: advisory lock: %w", err)
	}
	if !acquired {
		return fmt.Errorf("cortex: migration lock is held by another process")
	}

	query := fmt.Sprintf(`INSERT INTO %s (id, locked_at, locked_by)
		VALUES (1, NOW(), $1)
		ON CONFLICT (id) DO UPDATE SET locked_at = NOW(), locked_by = $1`,
		lockTableName)
	_, err := e.pgdb.Exec(ctx, query, lockedBy)
	return err
}

// ReleaseLock releases the distributed migration lock.
func (e *pgMigrateExecutor) ReleaseLock(ctx context.Context) error {
	query := fmt.Sprintf(`UPDATE %s SET locked_at = NULL, locked_by = NULL WHERE id = 1`,
		lockTableName)
	_, _ = e.pgdb.Exec(ctx, query)

	_, err := e.pgdb.Exec(ctx, "SELECT pg_advisory_unlock(1)")
	return err
}

// ListApplied returns all migrations that have been applied, ordered by id ascending.
func (e *pgMigrateExecutor) ListApplied(ctx context.Context) ([]*migrate.AppliedMigration, error) {
	query := fmt.Sprintf(
		`SELECT id, version, name, "group", migrated_at::text FROM %s ORDER BY id ASC`,
		migrationTableName)

	rows, err := e.pgdb.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var applied []*migrate.AppliedMigration
	for rows.Next() {
		a := &migrate.AppliedMigration{}
		if err := rows.Scan(&a.ID, &a.Version, &a.Name, &a.Group, &a.MigratedAt); err != nil {
			return nil, fmt.Errorf("cortex: scan applied migration: %w", err)
		}
		applied = append(applied, a)
	}
	return applied, rows.Err()
}

// RecordApplied records that a migration was successfully applied.
func (e *pgMigrateExecutor) RecordApplied(ctx context.Context, m *migrate.Migration) error {
	query := fmt.Sprintf(
		`INSERT INTO %s (version, name, "group") VALUES ($1, $2, $3)`,
		migrationTableName)
	_, err := e.pgdb.Exec(ctx, query, m.Version, m.Name, m.Group)
	return err
}

// RemoveApplied removes the record of an applied migration (for rollback).
func (e *pgMigrateExecutor) RemoveApplied(ctx context.Context, m *migrate.Migration) error {
	query := fmt.Sprintf(
		`DELETE FROM %s WHERE version = $1 AND "group" = $2`,
		migrationTableName)
	_, err := e.pgdb.Exec(ctx, query, m.Version, m.Group)
	return err
}
