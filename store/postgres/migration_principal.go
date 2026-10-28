package postgres

import (
	"context"

	"github.com/xraph/grove/migrate"
)

func init() {
	Migrations.MustRegister(&migrate.Migration{
		Name: "add_run_principal", Version: "20261101000001", Comment: "Keep a run's principal across a pause",
		Up: func(ctx context.Context, exec migrate.Executor) error {
			_, err := exec.Exec(ctx, "ALTER TABLE cortex_runs ADD COLUMN IF NOT EXISTS principal BYTEA")
			return err
		},
		Down: func(ctx context.Context, exec migrate.Executor) error {
			_, err := exec.Exec(ctx, "ALTER TABLE cortex_runs DROP COLUMN IF EXISTS principal")
			return err
		},
	})
}
