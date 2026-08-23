package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	"github.com/xraph/grove/migrate"
	"modernc.org/sqlite"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/store"
)

// Compile-time interface check.
var _ store.Store = (*Store)(nil)

// Store is a SQLite implementation of the composite Cortex store.
type Store struct {
	db  *grove.DB
	sdb *sqlitedriver.SqliteDB
}

// New creates a new SQLite store.
func New(db *grove.DB) *Store {
	return &Store{
		db:  db,
		sdb: sqlitedriver.Unwrap(db),
	}
}

// Migrate runs programmatic migrations via the grove orchestrator.
func (s *Store) Migrate(ctx context.Context, opts ...cortex.MigrateOption) error {
	o := cortex.ApplyMigrateOptions(opts...)

	executor, err := migrate.NewExecutorFor(s.sdb)
	if err != nil {
		return fmt.Errorf("cortex/sqlite: create migration executor: %w", err)
	}
	orch := migrate.NewOrchestrator(executor, Migrations)
	if _, err := orch.Migrate(ctx); err != nil {
		return fmt.Errorf("cortex/sqlite: migration failed: %w", err)
	}

	if err := s.rescopeLegacyRows(ctx, o); err != nil {
		return fmt.Errorf("cortex/sqlite: rescope legacy rows: %w", err)
	}

	// Runs after rescopeLegacyRows, unconditionally on every Migrate()
	// call, rather than only inside migration 20260824000004's one-shot
	// Up -- see that migration's Comment for the full reasoning (same as
	// the postgres store of the same version): a host jumping straight
	// from pre-v1.8.0 to this version in one Migrate() call has every
	// legacy conversation row sitting at scope_canon = '' at the point
	// the migration group runs, so a one-shot Up run before rescoping
	// finds nothing to backfill and, because grove never retries a
	// recorded version, never gets another chance. Calling this here
	// instead closes that gap and is safe on every boot: its own filter
	// (session_id = '' on kind = 'conversation') has nothing left to find
	// once a scope's rows have been backfilled once.
	if err := backfillDefaultSessions(ctx, executor); err != nil {
		return fmt.Errorf("cortex/sqlite: backfill default sessions: %w", err)
	}

	return nil
}

// Ping verifies the database connection.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.Ping(ctx)
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// isNoRows checks for the standard sql.ErrNoRows sentinel.
func isNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}

// SQLite extended result codes for unique-constraint violations.
const (
	sqliteConstraintUnique     = 2067 // SQLITE_CONSTRAINT_UNIQUE
	sqliteConstraintPrimaryKey = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
)

// isUniqueViolation reports whether err is a SQLite unique-constraint
// violation, so callers can translate it into cortex.ErrAlreadyExists.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() {
		case sqliteConstraintUnique, sqliteConstraintPrimaryKey:
			return true
		}
	}
	// Fallback in case the typed error is not surfaced by the driver.
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
