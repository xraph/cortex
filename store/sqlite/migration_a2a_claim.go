package sqlite

import (
	"context"
	"errors"

	"github.com/xraph/grove/migrate"
)

func init() {
	Migrations.MustRegister(&migrate.Migration{
		Name: "fence_a2a_delivery_claims", Version: "20261025000002", Comment: "Fence delivery run admission with unique claims",
		Up: func(ctx context.Context, exec migrate.Executor) error {
			columns, err := existingColumns(ctx, exec, "cortex_a2a_deliveries")
			if err != nil {
				return err
			}
			if !columns["claim_token"] {
				if _, err = exec.Exec(ctx, "ALTER TABLE cortex_a2a_deliveries ADD COLUMN claim_token TEXT NOT NULL DEFAULT ''"); err != nil {
					return err
				}
			}
			return nil
		},
		Down: func(context.Context, migrate.Executor) error {
			return errors.New("cortex: A2A claim migration cannot be reversed while executions may retain claims")
		},
	})
}
