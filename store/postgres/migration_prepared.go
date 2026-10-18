package postgres

import (
	"context"

	"github.com/xraph/grove/migrate"
)

func init() {
	Migrations.MustRegister(&migrate.Migration{
		Name: "add_run_provenance", Version: "20261018000002", Comment: "Retain prepared identity and cumulative accounting",
		Up: func(ctx context.Context, exec migrate.Executor) error {
			for _, column := range []struct{ name, fallback string }{
				{"provenance", "{}"}, {"usage", "{}"}, {"budget", "{}"},
			} {
				if _, err := exec.Exec(ctx, "ALTER TABLE cortex_runs ADD COLUMN IF NOT EXISTS "+column.name+" JSONB NOT NULL DEFAULT '"+column.fallback+"'"); err != nil {
					return err
				}
			}
			return nil
		},
		Down: func(ctx context.Context, exec migrate.Executor) error {
			for _, column := range []string{"budget", "usage", "provenance"} {
				if _, err := exec.Exec(ctx, "ALTER TABLE cortex_runs DROP COLUMN IF EXISTS "+column); err != nil {
					return err
				}
			}
			return nil
		},
	})
}
