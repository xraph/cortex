package postgres

import (
	"context"
	"errors"

	"github.com/xraph/grove/migrate"
)

func init() {
	Migrations.MustRegister(&migrate.Migration{
		Name: "isolate_a2a_orchestrations", Version: "20261025000001", Comment: "Partition A2A persistence and dispatch by orchestration",
		Up: func(ctx context.Context, exec migrate.Executor) error {
			for _, suffix := range []string{"messages", "conversations", "deliveries", "pending_asks"} {
				table := "cortex_a2a_" + suffix
				if _, err := exec.Exec(ctx, "ALTER TABLE "+table+" ADD COLUMN IF NOT EXISTS orchestration_id TEXT NOT NULL DEFAULT ''"); err != nil {
					return err
				}
				if _, err := exec.Exec(ctx, "CREATE INDEX IF NOT EXISTS idx_"+table+"_routing ON "+table+" (orchestration_id, scope_canon)"); err != nil {
					return err
				}
			}
			for _, ddl := range []string{
				"CREATE INDEX IF NOT EXISTS idx_a2a_routing_queue ON cortex_a2a_deliveries (orchestration_id, state, created_at, id)",
				"CREATE INDEX IF NOT EXISTS idx_a2a_routing_claim ON cortex_a2a_deliveries (orchestration_id, state, claimed_at)",
				"CREATE INDEX IF NOT EXISTS idx_a2a_routing_asks ON cortex_a2a_pending_asks (orchestration_id, claimed_at, deadline)",
			} {
				if _, err := exec.Exec(ctx, ddl); err != nil {
					return err
				}
			}
			return nil
		},
		Down: func(context.Context, migrate.Executor) error {
			return errors.New("cortex: A2A routing migration cannot be reversed without merging isolated traffic")
		},
	})
}
