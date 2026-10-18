package postgres

import (
	"context"

	"github.com/xraph/grove/migrate"
)

func init() {
	Migrations.MustRegister(&migrate.Migration{
		Name: "add_run_stop_reason", Version: "20261018000001", Comment: "Preserve execution termination without backfilling historical success",
		Up: func(ctx context.Context, exec migrate.Executor) error {
			for _, column := range []struct{ name, fallback string }{
				{"stop_reason", ""}, {"finish_reason", ""}, {"verification_status", "unchecked"}, {"result_ref", ""},
			} {
				if _, err := exec.Exec(ctx, "ALTER TABLE cortex_runs ADD COLUMN IF NOT EXISTS "+column.name+" TEXT NOT NULL DEFAULT '"+column.fallback+"'"); err != nil {
					return err
				}
			}
			return nil
		},
		Down: func(ctx context.Context, exec migrate.Executor) error {
			for _, column := range []string{"result_ref", "verification_status", "finish_reason", "stop_reason"} {
				if _, err := exec.Exec(ctx, "ALTER TABLE cortex_runs DROP COLUMN IF EXISTS "+column); err != nil {
					return err
				}
			}
			return nil
		},
	})
}
