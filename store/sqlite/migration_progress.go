package sqlite

import (
	"context"

	"github.com/xraph/grove/migrate"
)

func init() {
	Migrations.MustRegister(&migrate.Migration{
		Name: "create_run_progress", Version: "20261101000003", Comment: "Progress snapshots of running runs",
		Up: func(ctx context.Context, exec migrate.Executor) error {
			// SQLite enforces the foreign key only on a connection that turned
			// foreign_keys on, and the driver turns it on for one pooled
			// connection. The trigger removes a run's progress with the run on
			// every connection, so a deleted run never leaves a snapshot behind.
			_, err := exec.Exec(ctx, `
CREATE TABLE IF NOT EXISTS cortex_run_progress (
    run_id       TEXT PRIMARY KEY REFERENCES cortex_runs(id) ON DELETE CASCADE,
    owner        TEXT NOT NULL,
    progress     TEXT NOT NULL,
    scope_l0     TEXT NOT NULL DEFAULT '',
    scope_l1     TEXT NOT NULL DEFAULT '',
    scope_l2     TEXT NOT NULL DEFAULT '',
    scope_extra  TEXT NOT NULL DEFAULT '{}',
    scope_canon  TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_cortex_run_progress_scope
    ON cortex_run_progress (scope_l0, scope_l1, scope_l2);

CREATE TRIGGER IF NOT EXISTS trg_cortex_run_progress_cascade
    AFTER DELETE ON cortex_runs
BEGIN
    DELETE FROM cortex_run_progress WHERE run_id = OLD.id;
END;
`)
			return err
		},
		Down: func(ctx context.Context, exec migrate.Executor) error {
			_, err := exec.Exec(ctx, `
DROP TRIGGER IF EXISTS trg_cortex_run_progress_cascade;
DROP TABLE IF EXISTS cortex_run_progress;
`)
			return err
		},
	})
}
