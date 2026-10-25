package sqlite

import (
	"context"
	"errors"

	"github.com/xraph/grove/migrate"
)

func init() {
	Migrations.MustRegister(&migrate.Migration{
		Name: "serialize_orchestration_children", Version: "20261025000003", Comment: "Retain child identities across concurrent orchestration writes",
		Up: func(ctx context.Context, exec migrate.Executor) error {
			columns, err := existingColumns(ctx, exec, "cortex_orchestration_runs")
			if err != nil {
				return err
			}
			if !columns["revision"] {
				if _, err = exec.Exec(ctx, "ALTER TABLE cortex_orchestration_runs ADD COLUMN revision INTEGER NOT NULL DEFAULT 0"); err != nil {
					return err
				}
			}
			return nil
		},
		Down: func(context.Context, migrate.Executor) error {
			return errors.New("cortex: orchestration revisions cannot be removed while writers retain snapshots")
		},
	})
}
