package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate"
	"github.com/xraph/grove/migrate"
)

// TestMigrate_idempotentAfterPartialFailure reproduces the scenario the
// deferred finding described: a scope-column migration adds its ALTER TABLE
// columns successfully but crashes (or the process is killed) before grove
// records the version as applied. On the next boot, the orchestrator sees
// the migration as unapplied and reruns it. Before the addScopeColumns fix,
// this aborted with "duplicate column name" and left the database stuck.
func TestMigrate_idempotentAfterPartialFailure(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "cortex_migrate_test.db")

	drv := sqlitedriver.New()
	if err := drv.Open(ctx, dsn); err != nil {
		t.Fatalf("open sqlite driver: %v", err)
	}
	db, openErr := grove.Open(drv)
	if openErr != nil {
		t.Fatalf("grove open: %v", openErr)
	}
	s := New(db)
	t.Cleanup(func() { _ = s.Close() })

	// First run: full success, every migration applied and recorded.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("first migrate: %v", err)
	}

	// Simulate "columns landed, version never recorded": delete the applied
	// rows for the two scope-column migrations without touching the columns
	// they already added.
	for _, version := range []string{"20260821000001", "20260822000001"} {
		if _, err := s.sdb.Exec(ctx,
			`DELETE FROM grove_migrations WHERE version = ? AND "group" = 'cortex'`, version); err != nil {
			t.Fatalf("simulate unrecorded version %s: %v", version, err)
		}
	}

	// Second run: the orchestrator thinks both migrations are still
	// pending and reruns them against tables that already have the
	// columns. This must be a clean no-op, not a "duplicate column name"
	// error.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("second migrate (rerun after simulated partial failure): %v", err)
	}

	// Third run for good measure: fully normal idempotent rerun (grove
	// sees every version as applied and skips them all).
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("third migrate: %v", err)
	}

	// Confirm the scope columns exist exactly once and are usable.
	executor, execErr := migrate.NewExecutorFor(s.sdb)
	if execErr != nil {
		t.Fatalf("create migration executor: %v", execErr)
	}
	for _, table := range []string{
		"cortex_agents", "cortex_runs", "cortex_memories", "cortex_checkpoints",
		"cortex_steps", "cortex_tool_calls",
	} {
		cols, colsErr := existingColumns(ctx, executor, table)
		if colsErr != nil {
			t.Fatalf("read columns of %s: %v", table, colsErr)
		}
		for _, col := range scopeColumnDefs {
			if !cols[col.name] {
				t.Errorf("table %s missing column %s after rerun", table, col.name)
			}
		}
	}
}
