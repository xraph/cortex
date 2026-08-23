package postgres

import (
	"context"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/pgdriver"
)

// TestMigrate_workingMemoryIndexRecoversFromInvalidBuild reproduces the
// failure mode a review round flagged in the scope_working_memory_unique_index
// migration: a CONCURRENTLY index build that fails partway leaves an INVALID
// index under the target name. Before this fix, the CREATE carried
// IF NOT EXISTS, so a retry saw the name already taken, emitted a NOTICE, and
// returned success without ever building a working index -- silently
// reopening the cross-scope working-memory clobber this migration exists to
// close, with grove recording the migration as applied.
//
// A live "start a build, kill the connection mid-flight" race is possible in
// principle but isn't reliably deterministic in a test. This instead forces
// Postgres into the exact same terminal state -- an index row with
// indisvalid = false under the target name -- by making the CONCURRENTLY
// build fail on a uniqueness violation, which is one of the real ways
// (alongside a killed connection or a lock-wait timeout) that Postgres
// produces an invalid concurrently-built index. Once the index exists with
// indisvalid = false, Postgres does not care why it got that way; the
// migration's recovery path is identical either way.
func TestMigrate_workingMemoryIndexRecoversFromInvalidBuild(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	pgContainer, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("cortex_migrate_invalid_index"),
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
		t.Fatalf("initial migrate: %v", err)
	}

	// Confirm the index landed valid and scope-aware from the normal path
	// before breaking anything.
	assertWorkingMemoryIndex(ctx, t, s, true, 4)

	// Simulate "the migration ran before, but the concurrent build failed
	// partway and grove never recorded it as applied": roll back the
	// bookkeeping row for this migration only, so the orchestrator treats
	// it as pending again on the next Migrate call, exactly like it would
	// after a crash between the failed Exec and RecordApplied.
	if _, err := s.pgdb.Exec(ctx,
		`DELETE FROM grove_migrations WHERE version = $1 AND "group" = 'cortex'`,
		"20260822000002"); err != nil {
		t.Fatalf("simulate unrecorded version: %v", err)
	}

	// Drop the valid index (mirrors the migration's own unconditional
	// DROP) and insert a duplicate row that violates the intended unique
	// index, so the next CONCURRENTLY build fails on the validation scan
	// and Postgres leaves an INVALID index under the name -- the exact
	// terminal state a killed or timed-out build would also leave.
	if _, err := s.pgdb.Exec(ctx, `DROP INDEX CONCURRENTLY IF EXISTS idx_cortex_memories_working`); err != nil {
		t.Fatalf("drop index to stage failure: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := s.pgdb.Exec(ctx,
			`INSERT INTO cortex_memories (agent_id, kind, key, content, metadata, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon)
			 VALUES ('agent-dupe', 'working', 'k', '{}', '{}', 'tenant=acme', '', '', '{}', 'tenant=acme')`); err != nil {
			t.Fatalf("insert duplicate row %d: %v", i, err)
		}
	}

	buildErr := func() error {
		_, err := s.pgdb.Exec(ctx,
			`CREATE UNIQUE INDEX CONCURRENTLY idx_cortex_memories_working ON cortex_memories (agent_id, kind, key, scope_canon) WHERE kind = 'working'`)
		return err
	}()
	if buildErr == nil {
		t.Fatalf("expected the staged duplicate rows to fail the concurrent build, but it succeeded")
	}
	t.Logf("staged build failed as expected: %v", buildErr)

	// Confirm Postgres actually left an invalid index behind, not no
	// index at all -- this is the state the fix has to recover from.
	assertWorkingMemoryIndex(ctx, t, s, false, 4)

	// Clean up what made the staged build fail, representing whatever
	// transient condition caused the real failure (lock contention,
	// connection blip) no longer being present on retry.
	if _, err := s.pgdb.Exec(ctx, `DELETE FROM cortex_memories WHERE agent_id = 'agent-dupe'`); err != nil {
		t.Fatalf("clean up duplicate rows: %v", err)
	}

	// The retry: grove thinks this migration is still pending (its record
	// was deleted above) and an INVALID index already sits under the
	// target name. Before the fix, IF NOT EXISTS on the CREATE would have
	// made this return nil without ever building a working index.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("retry migrate after invalid index: %v", err)
	}

	// The retry must have dropped the invalid index and built a real,
	// valid, scope-aware one -- not silently left the invalid one in
	// place.
	assertWorkingMemoryIndex(ctx, t, s, true, 4)
}

// assertWorkingMemoryIndex checks idx_cortex_memories_working's validity and
// column count via pg_index/pg_class, failing the test if either doesn't
// match.
func assertWorkingMemoryIndex(ctx context.Context, t *testing.T, s *Store, wantValid bool, wantColumns int) {
	t.Helper()
	row := s.pgdb.QueryRow(ctx, `
SELECT ix.indisvalid, ix.indnkeyatts
FROM pg_class c
JOIN pg_index ix ON ix.indexrelid = c.oid
WHERE c.relname = 'idx_cortex_memories_working'
`)
	var (
		valid   bool
		nKeyAtt int16
	)
	if err := row.Scan(&valid, &nKeyAtt); err != nil {
		t.Fatalf("read idx_cortex_memories_working state: %v", err)
	}
	if valid != wantValid {
		t.Errorf("idx_cortex_memories_working indisvalid = %v, want %v", valid, wantValid)
	}
	if int(nKeyAtt) != wantColumns {
		t.Errorf("idx_cortex_memories_working has %d key columns, want %d", nKeyAtt, wantColumns)
	}
}
