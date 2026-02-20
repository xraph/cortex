// Package postgres provides a PostgreSQL implementation of the Cortex
// composite store using bun ORM with embedded SQL migrations.
package postgres

import (
	"context"
	"embed"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/store"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Compile-time interface check.
var _ store.Store = (*Store)(nil)

// Store is a PostgreSQL implementation of the composite Cortex store.
type Store struct {
	db *bun.DB
}

// New creates a new PostgreSQL store.
func New(db *bun.DB) *Store {
	return &Store{db: db}
}

// Migrate runs embedded SQL migrations.
func (s *Store) Migrate(ctx context.Context) error {
	files, err := migrations.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("cortex: %w: %w", cortex.ErrMigrationFailed, err)
	}

	for _, f := range files {
		data, err := migrations.ReadFile("migrations/" + f.Name())
		if err != nil {
			return fmt.Errorf("cortex: %w: read %s: %w", cortex.ErrMigrationFailed, f.Name(), err)
		}
		if _, err := s.db.ExecContext(ctx, string(data)); err != nil {
			return fmt.Errorf("cortex: %w: exec %s: %w", cortex.ErrMigrationFailed, f.Name(), err)
		}
	}
	return nil
}

// Ping verifies the database connection.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}
