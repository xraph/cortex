package postgres

import (
	"context"
	"fmt"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/pgdriver"

	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/storetest"
)

// TestConformance runs the backend-agnostic scope-isolation contract
// (store/storetest) against a real PostgreSQL instance via
// testcontainers-go. This is the first time these scope guards — the
// ON CONFLICT upsert in SaveWorking included — actually execute against
// Postgres rather than being verified by reading sqlite-identical code.
//
// SkipIfProviderIsNotHealthy makes this skip cleanly (not fail) when
// Docker isn't available, so a plain `go test ./...` still passes on a
// machine without it, while running for real whenever Docker is present.
func TestConformance(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	pgContainer, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("cortex_conformance"),
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
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// One container serves the whole run: spinning up a fresh postgres
	// instance per subtest would make this suite minutes slower for no
	// isolation benefit truncateAll doesn't already provide.
	storetest.Conformance(t, func(t *testing.T) store.Store {
		t.Helper()
		if err := truncateConformanceTables(ctx, s); err != nil {
			t.Fatalf("truncate before subtest: %v", err)
		}
		return s
	})
}

// truncateConformanceTables clears every table store/storetest touches so
// each newStore(t) call hands Conformance a "freshly migrated, empty
// store" per its contract, without paying for a new container or database
// per subtest. RESTART IDENTITY isn't load-bearing (every PK here is a
// TypeID, not a sequence) but costs nothing and keeps this honest if that
// ever changes.
//
// This list used to stop at cortex_agents, from when that was every
// entity Conformance exercised. It went stale as skills, traits,
// behaviors, personas, sessions, and orchestration were added without a
// matching entry here — latent, because no subtest before now both wrote
// one of those tables AND ran an unfiltered list against it later in the
// same TestConformance run. storetest's own SessionMessageCounters and
// Conversation subtests do exactly that for cortex_sessions (writing
// stray rows another subtest's ListSessions then picked up), which is
// what surfaced the gap; the fix is to stop it recurring for the rest.
func truncateConformanceTables(ctx context.Context, s *Store) error {
	const stmt = `TRUNCATE TABLE
		cortex_tool_calls,
		cortex_steps,
		cortex_checkpoints,
		cortex_memories,
		cortex_runs,
		cortex_agents,
		cortex_skills,
		cortex_traits,
		cortex_behaviors,
		cortex_personas,
		cortex_sessions,
		cortex_suspensions,
		cortex_overlays,
		cortex_orchestration_configs,
		cortex_orchestration_runs
	RESTART IDENTITY CASCADE`
	if _, err := s.pgdb.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("truncate conformance tables: %w", err)
	}
	return nil
}
