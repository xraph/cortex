// Package postgres persists optimization state with PostgreSQL transactions.
package postgres

import (
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/pgdriver"

	"github.com/xraph/cortex/optimization/store/internal/sqlstore"
	"github.com/xraph/cortex/optimization/store/internal/state"
)

// Store applies shared optimization rules to exact-scope PostgreSQL records.
type Store struct{ *state.Store }

// New binds an open Grove PostgreSQL connection. Call Migrate before serving work.
func New(db *grove.DB) *Store {
	backend := sqlstore.New(db, pgdriver.Unwrap(db), sqlstore.PostgreSQL)
	return &Store{Store: state.NewBackend(backend)}
}
