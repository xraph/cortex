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
func (s *Store) Migrate(ctx context.Context) error {
	executor := &pgMigrateExecutor{pgdb: s.pgdb}

	orch := migrate.NewOrchestrator(executor, Migrations)
	if _, err := orch.Migrate(ctx); err != nil {
		return fmt.Errorf("cortex: migration failed: %w", err)
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
