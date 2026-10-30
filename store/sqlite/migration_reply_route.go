package sqlite

import (
	"context"

	"github.com/xraph/grove/migrate"
)

func init() {
	Migrations.MustRegister(&migrate.Migration{
		Name: "add_run_reply_route", Version: "20261101000004", Comment: "Keep a directive run's reply route out of its metadata",
		Up: func(ctx context.Context, exec migrate.Executor) error {
			columns, err := existingColumns(ctx, exec, "cortex_runs")
			if err != nil || columns["reply_route"] {
				return err
			}
			_, err = exec.Exec(ctx, "ALTER TABLE cortex_runs ADD COLUMN reply_route BLOB")
			return err
		},
		Down: func(ctx context.Context, exec migrate.Executor) error {
			_, err := exec.Exec(ctx, "ALTER TABLE cortex_runs DROP COLUMN reply_route")
			return err
		},
	})
}
