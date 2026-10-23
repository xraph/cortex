package postgres

import (
	"context"

	"github.com/xraph/grove/migrate"
)

func init() {
	Migrations.MustRegister(&migrate.Migration{
		Name: "add_run_revision", Version: "20261023000001", Comment: "Fence concurrent run updates",
		Up: func(ctx context.Context, exec migrate.Executor) error {
			_, err := exec.Exec(ctx, "ALTER TABLE cortex_runs ADD COLUMN IF NOT EXISTS revision BIGINT NOT NULL DEFAULT 0 CHECK (revision >= 0)")
			return err
		},
		Down: func(ctx context.Context, exec migrate.Executor) error {
			_, err := exec.Exec(ctx, "ALTER TABLE cortex_runs DROP COLUMN IF EXISTS revision")
			return err
		},
	})
}
