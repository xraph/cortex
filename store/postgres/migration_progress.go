package postgres

import (
	"context"

	"github.com/xraph/grove/migrate"
)

func init() {
	Migrations.MustRegister(&migrate.Migration{
		Name: "create_run_progress", Version: "20261101000003", Comment: "Progress snapshots of running runs",
		Up: func(ctx context.Context, exec migrate.Executor) error {
			_, err := exec.Exec(ctx, `
CREATE TABLE IF NOT EXISTS cortex_run_progress (
    run_id       TEXT PRIMARY KEY REFERENCES cortex_runs(id) ON DELETE CASCADE,
    owner        TEXT NOT NULL,
    progress     JSONB NOT NULL,
    scope_l0     TEXT NOT NULL DEFAULT '',
    scope_l1     TEXT NOT NULL DEFAULT '',
    scope_l2     TEXT NOT NULL DEFAULT '',
    scope_extra  JSONB NOT NULL DEFAULT '{}',
    scope_canon  TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_cortex_run_progress_scope
    ON cortex_run_progress (scope_l0, scope_l1, scope_l2);
`)
			return err
		},
		Down: func(ctx context.Context, exec migrate.Executor) error {
			_, err := exec.Exec(ctx, `DROP TABLE IF EXISTS cortex_run_progress`)
			return err
		},
	})
}
