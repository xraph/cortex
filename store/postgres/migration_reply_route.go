package postgres

import (
	"context"

	"github.com/xraph/grove/migrate"
)

func init() {
	Migrations.MustRegister(&migrate.Migration{
		Name: "add_run_reply_route", Version: "20261101000004", Comment: "Keep a directive run's reply route out of its metadata",
		Up: func(ctx context.Context, exec migrate.Executor) error {
			_, err := exec.Exec(ctx, "ALTER TABLE cortex_runs ADD COLUMN IF NOT EXISTS reply_route BYTEA")
			return err
		},
		Down: func(ctx context.Context, exec migrate.Executor) error {
			_, err := exec.Exec(ctx, "ALTER TABLE cortex_runs DROP COLUMN IF EXISTS reply_route")
			return err
		},
	})
}
