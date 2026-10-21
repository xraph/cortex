// Package sqlite persists optimization state in SQLite through Grove transactions.
package sqlite

import (
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"

	"github.com/xraph/cortex/optimization/store/internal/sqlstore"
	"github.com/xraph/cortex/optimization/store/internal/state"
)

// Store applies shared optimization rules to an exact-scope SQLite record store.
type Store struct{ *state.Store }

// New binds an open Grove SQLite connection. Call Migrate before serving work.
// Configure WAL and a busy timeout on the connection for concurrent workers.
func New(db *grove.DB) *Store {
	backend := sqlstore.New(db, sqlitedriver.Unwrap(db), sqlstore.SQLite)
	return &Store{Store: state.NewBackend(backend)}
}
