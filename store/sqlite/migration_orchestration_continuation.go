package sqlite

import (
	"context"
	"errors"

	"github.com/xraph/grove/migrate"
)

func init() {
	Migrations.MustRegister(&migrate.Migration{
		Name: "fence_orchestration_continuations", Version: "20261025000004", Comment: "Fence coordinator snapshots across waiting passes",
		Up: func(ctx context.Context, exec migrate.Executor) error {
			columns, err := existingColumns(ctx, exec, "cortex_orchestration_runs")
			if err != nil {
				return err
			}
			if !columns["continuation_generation"] {
				if _, err = exec.Exec(ctx, "ALTER TABLE cortex_orchestration_runs ADD COLUMN continuation_generation INTEGER NOT NULL DEFAULT 0"); err != nil {
					return err
				}
			}
			return nil
		},
		Down: func(context.Context, migrate.Executor) error {
			return errors.New("cortex: continuation generations cannot be removed while writers retain snapshots")
		},
	})
}
