package sqlite

import (
	"context"

	"github.com/xraph/grove/migrate"
)

// continuationColumns are the cortex_runs columns run.ContinuationStore
// writes, in the order the migration adds them.
var continuationColumns = []struct{ name, definition string }{
	{"lease_owner", "TEXT NOT NULL DEFAULT ''"},
	{"lease_expires_at", "TEXT"},
	{"ready", "TEXT"},
	{"reclaim_count", "INTEGER NOT NULL DEFAULT 0"},
	{"notified_at", "TEXT"},
}

func init() {
	Migrations.MustRegister(&migrate.Migration{
		Name: "add_run_continuation", Version: "20261101000002", Comment: "Ready and reclaimable runs and worker leases",
		Up: func(ctx context.Context, exec migrate.Executor) error {
			columns, err := existingColumns(ctx, exec, "cortex_runs")
			if err != nil {
				return err
			}
			for _, column := range continuationColumns {
				if columns[column.name] {
					continue
				}
				if _, err = exec.Exec(ctx, "ALTER TABLE cortex_runs ADD COLUMN "+column.name+" "+column.definition); err != nil {
					return err
				}
			}
			_, err = exec.Exec(ctx, `
CREATE INDEX IF NOT EXISTS idx_cortex_runs_lapsed ON cortex_runs (lease_expires_at)
    WHERE state = 'running' AND lease_expires_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_cortex_runs_continuable ON cortex_runs (updated_at)
    WHERE state IN ('ready', 'reclaimable');
`)
			return err
		},
		Down: func(ctx context.Context, exec migrate.Executor) error {
			if _, err := exec.Exec(ctx, "DROP INDEX IF EXISTS idx_cortex_runs_lapsed; DROP INDEX IF EXISTS idx_cortex_runs_continuable"); err != nil {
				return err
			}
			for i := len(continuationColumns) - 1; i >= 0; i-- {
				if _, err := exec.Exec(ctx, "ALTER TABLE cortex_runs DROP COLUMN "+continuationColumns[i].name); err != nil {
					return err
				}
			}
			return nil
		},
	})
}
