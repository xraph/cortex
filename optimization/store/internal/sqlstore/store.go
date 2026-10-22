// Package sqlstore shares transactional record persistence across SQL backends.
package sqlstore

import (
	"context"
	"strconv"
	"strings"

	"github.com/xraph/grove"
	"github.com/xraph/grove/driver"

	"github.com/xraph/cortex/optimization/store/internal/state"
)

// Kind selects the supported database's transaction and SQL conventions.
type Kind int

const (
	// SQLite uses question-mark parameters and a database-wide writer lock.
	SQLite Kind = iota
	// PostgreSQL uses numbered parameters and row locks under READ COMMITTED.
	PostgreSQL
)

type backend struct {
	db  *grove.DB
	sql *connection
}

// New binds a database connection to the shared record adapter.
func New(db *grove.DB, raw driver.Driver, kind Kind) state.Backend {
	if kind != SQLite && kind != PostgreSQL {
		panic("optimization: unsupported internal SQL backend")
	}
	return &backend{db: db, sql: &connection{Driver: raw, kind: kind}}
}

func (b *backend) Ping(ctx context.Context) error { return b.db.Ping(ctx) }
func (b *backend) Close() error                   { return b.db.Close() }

type connection struct {
	driver.Driver
	kind Kind
}

// statement only receives this package's static templates. Caller data always
// remains in driver arguments, never in the rewritten SQL.
func statement(kind Kind, query string) string {
	if kind == SQLite {
		return query
	}
	query = strings.ReplaceAll(query, " BLOB ", " BYTEA ")
	var result strings.Builder
	index := 1
	for _, char := range query {
		if char == '?' {
			result.WriteByte('$')
			result.WriteString(strconv.Itoa(index))
			index++
		} else {
			result.WriteRune(char)
		}
	}
	return result.String()
}

func (c *connection) Exec(ctx context.Context, query string, args ...any) (driver.Result, error) {
	return c.Driver.Exec(ctx, statement(c.kind, query), args...)
}

func (c *connection) Query(ctx context.Context, query string, args ...any) (driver.Rows, error) {
	return c.Driver.Query(ctx, statement(c.kind, query), args...)
}

func (c *connection) QueryRow(ctx context.Context, query string, args ...any) driver.Row {
	return c.Driver.QueryRow(ctx, statement(c.kind, query), args...)
}

func (c *connection) BeginTx(ctx context.Context, options *driver.TxOptions) (driver.Tx, error) {
	if c.kind == PostgreSQL {
		copyOptions := driver.TxOptions{}
		if options != nil {
			copyOptions = *options
		}
		copyOptions.IsolationLevel = driver.LevelReadCommitted
		options = &copyOptions
	}
	tx, err := c.Driver.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &transaction{Tx: tx, kind: c.kind}, nil
}

type transaction struct {
	driver.Tx
	kind Kind
}

func (tx *transaction) Exec(ctx context.Context, query string, args ...any) (driver.Result, error) {
	return tx.Tx.Exec(ctx, statement(tx.kind, query), args...)
}

func (tx *transaction) Query(ctx context.Context, query string, args ...any) (driver.Rows, error) {
	return tx.Tx.Query(ctx, statement(tx.kind, query), args...)
}

func (tx *transaction) QueryRow(ctx context.Context, query string, args ...any) driver.Row {
	return tx.Tx.QueryRow(ctx, statement(tx.kind, query), args...)
}
