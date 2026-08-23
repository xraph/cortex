// Package postgres provides a PostgreSQL implementation of the Cortex
// composite store using grove ORM with programmatic migrations.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/pgdriver"
	"github.com/xraph/grove/migrate"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/store"
)

// Compile-time interface check.
var _ store.Store = (*Store)(nil)

// Store is a PostgreSQL implementation of the composite Cortex store.
type Store struct {
	db   *grove.DB
	pgdb *pgdriver.PgDB
}

// New creates a new PostgreSQL store.
func New(db *grove.DB) *Store {
	return &Store{
		db:   db,
		pgdb: pgdriver.Unwrap(db),
	}
}

// Migrate runs programmatic migrations via the grove orchestrator.
func (s *Store) Migrate(ctx context.Context, opts ...cortex.MigrateOption) error {
	o := cortex.ApplyMigrateOptions(opts...)

	executor := &pgMigrateExecutor{pgdb: s.pgdb}

	orch := migrate.NewOrchestrator(executor, Migrations)
	if _, err := orch.Migrate(ctx); err != nil {
		return fmt.Errorf("cortex: migration failed: %w", err)
	}

	if err := s.rescopeLegacyRows(ctx, o); err != nil {
		return fmt.Errorf("cortex: rescope legacy rows: %w", err)
	}

	// Runs after rescopeLegacyRows, unconditionally on every Migrate()
	// call, rather than only inside migration 20260824000004's one-shot
	// Up (see that migration's Comment for why the one-shot version was
	// wrong): a host jumping straight from pre-v1.8.0 to this version in
	// one Migrate() call has every legacy conversation row sitting at
	// scope_canon = '' at the point the migration group runs, so a
	// one-shot Up run before rescoping finds nothing to backfill and,
	// because grove never retries a recorded version, never gets another
	// chance -- those rows would stay scoped (reachable to every other
	// query) but permanently orphaned from a session. Calling this here
	// instead closes that gap: it runs after every row in this call has
	// its real scope, and it is safe to run on every boot because its
	// own filter (session_id = '' on kind = 'conversation') has nothing
	// left to find once a scope's rows have been backfilled once.
	if err := backfillDefaultSessions(ctx, executor); err != nil {
		return fmt.Errorf("cortex: backfill default sessions: %w", err)
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

// pgUniqueViolation is the PostgreSQL SQLSTATE code for unique_violation.
const pgUniqueViolation = "23505"

// isUniqueViolation reports whether err is a PostgreSQL unique-constraint
// violation, so callers can translate it into cortex.ErrAlreadyExists.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == pgUniqueViolation
	}
	return false
}
