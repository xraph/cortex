package postgres

import (
	"context"

	"github.com/xraph/grove/migrate"
)

func init() {
	Migrations.MustRegister(&migrate.Migration{
		Name: "add_run_continuation", Version: "20261101000002", Comment: "Ready and reclaimable runs and worker leases",
		Up: func(ctx context.Context, exec migrate.Executor) error {
			_, err := exec.Exec(ctx, `
ALTER TABLE cortex_runs
    ADD COLUMN IF NOT EXISTS lease_owner TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS lease_expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS ready JSONB,
    ADD COLUMN IF NOT EXISTS reclaim_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS notified_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_cortex_runs_lapsed ON cortex_runs (lease_expires_at)
    WHERE state = 'running' AND lease_expires_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_cortex_runs_continuable ON cortex_runs (updated_at)
    WHERE state IN ('ready', 'reclaimable');
`)
			return err
		},
		Down: func(ctx context.Context, exec migrate.Executor) error {
			_, err := exec.Exec(ctx, `
DROP INDEX IF EXISTS idx_cortex_runs_lapsed;
DROP INDEX IF EXISTS idx_cortex_runs_continuable;
ALTER TABLE cortex_runs
    DROP COLUMN IF EXISTS notified_at,
    DROP COLUMN IF EXISTS reclaim_count,
    DROP COLUMN IF EXISTS ready,
    DROP COLUMN IF EXISTS lease_expires_at,
    DROP COLUMN IF EXISTS lease_owner;
`)
			return err
		},
	})
}
