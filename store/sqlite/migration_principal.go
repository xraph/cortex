package sqlite

import (
	"context"

	"github.com/xraph/grove/migrate"
)

func init() {
	Migrations.MustRegister(&migrate.Migration{
		Name: "add_run_principal", Version: "20261101000001", Comment: "Keep a run's principal across a pause",
		Up: func(ctx context.Context, exec migrate.Executor) error {
			columns, err := existingColumns(ctx, exec, "cortex_runs")
			if err != nil || columns["principal"] {
				return err
			}
			_, err = exec.Exec(ctx, "ALTER TABLE cortex_runs ADD COLUMN principal BLOB")
			return err
		},
		Down: func(ctx context.Context, exec migrate.Executor) error {
			_, err := exec.Exec(ctx, "ALTER TABLE cortex_runs DROP COLUMN principal")
			return err
		},
	})
}
