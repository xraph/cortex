// Package journal opens private, exclusively owned SQLite journals.
package journal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite" // Local durable worker and broker state.
)

// Journal keeps one connection and its exclusive lock until Close. Callers
// serialize their own state transitions; a second process cannot take ownership
// while this connection is alive. Protect the directory against owner mutation.
type Journal struct {
	DB   *sql.DB
	Conn *sql.Conn
}

// Application prevents a broker from opening a worker journal or the reverse.
type Application int

const (
	// Broker identifies the broker receipt/status journal.
	Broker Application = 0x43584252
	// Worker identifies the worker ownership journal.
	Worker Application = 0x4358574b
)

// Open checks the private file and synchronously acquires exclusive ownership.
func Open(ctx context.Context, filename string, application Application) (*Journal, error) {
	if application != Broker && application != Worker {
		return nil, fmt.Errorf("cortex: unknown journal application")
	}
	path, err := preparePath(filename)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	j := &Journal{DB: db, Conn: conn}
	for _, statement := range []string{"PRAGMA busy_timeout=0", "PRAGMA locking_mode=EXCLUSIVE", "PRAGMA synchronous=FULL", "PRAGMA journal_mode=DELETE", "BEGIN EXCLUSIVE", "COMMIT"} {
		if _, err = conn.ExecContext(ctx, statement); err != nil {
			return nil, errors.Join(fmt.Errorf("cortex: open sandbox journal: %w", err), j.Close())
		}
	}
	var existing Application
	if err = conn.QueryRowContext(ctx, "PRAGMA application_id").Scan(&existing); err != nil {
		return nil, errors.Join(err, j.Close())
	}
	if existing != 0 && existing != application {
		return nil, errors.Join(fmt.Errorf("cortex: journal belongs to another application"), j.Close())
	}
	if existing == 0 {
		if _, err = conn.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id=%d", application)); err != nil {
			return nil, errors.Join(err, j.Close())
		}
	}
	return j, nil
}

// Close releases ownership without deleting persisted state.
func (j *Journal) Close() error { return errors.Join(j.Conn.Close(), j.DB.Close()) }
