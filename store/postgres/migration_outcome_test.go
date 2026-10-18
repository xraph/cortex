package postgres

import (
	"context"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/pgdriver"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

func TestRunOutcomeMigrationPreservesHistoricalCompletion(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	pgContainer, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("cortex_migrate_outcomes"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := pgContainer.Terminate(context.Background()); cleanupErr != nil {
			t.Logf("terminate postgres container: %v", cleanupErr)
		}
	})

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres connection string: %v", err)
	}

	drv := pgdriver.New()
	if err = drv.Open(ctx, dsn); err != nil {
		t.Fatalf("open pg driver: %v", err)
	}
	db, err := grove.Open(drv)
	if err != nil {
		t.Fatalf("grove open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s := New(db)
	// Schema-only: an empty database has no unscoped rows, so this needs
	// no Rescoper and both rescopeLegacyRows and backfillDefaultSessions
	// are no-ops here. Tables have to exist before the raw SQL inserts
	// below can write to them.
	if err = s.Migrate(ctx); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}

	ctx = cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "migration"}}})
	// Reconstruct the prior schema after creating a legacy-shaped run. The old
	// columns and data survive while the additive migration is replayed.
	ag := &agent.Config{ID: id.NewAgentID(), Name: "historical"}
	if err := s.Create(ctx, ag); err != nil {
		t.Fatal(err)
	}
	r := &run.Run{ID: id.NewAgentRunID(), AgentID: ag.ID, State: run.StateCompleted, Output: "old answer"}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"stop_reason", "finish_reason", "verification_status", "result_ref"} {
		if _, err := s.pgdb.Exec(ctx, "ALTER TABLE cortex_runs DROP COLUMN "+column); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, err := s.pgdb.Exec(ctx, `DELETE FROM grove_migrations WHERE version = $1 AND "group" = 'cortex'`, "20261018000001"); err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != run.StateCompleted || got.Output != "old answer" || got.StopReason != "" || got.VerificationStatus != verification.StatusUnchecked {
			t.Fatalf("migration fabricated assurance: %+v", got)
		}
	}
}
