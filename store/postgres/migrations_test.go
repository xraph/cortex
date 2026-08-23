package postgres

import (
	"context"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/pgdriver"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/session"
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

// TestMigrate_backfillDefaultSessions proves the 20260824000004 backfill
// against a genuinely pre-v1.9.0 shape, not an empty database: rows are
// inserted directly with SQL, bypassing SaveConversation entirely (which
// always stamps a real session_id today and so could never produce the
// shape this migration exists to repair).
//
// Three rows go into cortex_memories for one (agent_id, scope) pair, and
// two of them simulate the pre-daa7e44 duplication bug this phase's
// prior task uncovered: the reasoning loop used to re-save a run's
// entire reloaded history on every turn, so the same logical message can
// appear as more than one physical row with a different Timestamp field
// but identical Role/Content -- see backfillDefaultSessions' comment in
// migrations.go for the full reasoning behind counting message_count as
// distinct (role, content) pairs instead of a raw row count. A fourth
// row is left at scope_canon = ” to prove the "no Rescoper ran"
// skip path leaves rows exactly as unreachable as they already were.
func TestMigrate_backfillDefaultSessions(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	pgContainer, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("cortex_migrate_backfill_sessions"),
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
	if err = s.Migrate(ctx); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}

	agentID := id.NewAgentID()
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_legacy"}}}
	canon := scope.Canonical() // "workspace=ws_legacy"

	// Two logically distinct messages, the first duplicated once with a
	// different Timestamp -- exactly the shape engine.llmToMemory's
	// reload-then-resave round trip produced pre-daa7e44. Raw
	// content-string equality would see three different rows here; the
	// backfill's Role+Content dedup must still land on 2.
	legacyRows := []string{
		`{"role":"user","content":"hello","timestamp":"2024-01-01T00:00:00Z"}`,
		`{"role":"assistant","content":"hi there","timestamp":"2024-01-01T00:00:01Z"}`,
		`{"role":"user","content":"hello","timestamp":"2024-01-02T00:00:00Z"}`, // duplicate resave
	}
	for _, content := range legacyRows {
		if _, err = s.pgdb.Exec(ctx, `
INSERT INTO cortex_memories (agent_id, session_id, kind, content, metadata, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon)
VALUES ($1, '', 'conversation', $2, '{}', $3, '', '', '{}', $4)`,
			agentID.String(), content, canon, canon,
		); err != nil {
			t.Fatalf("insert legacy conversation row: %v", err)
		}
	}

	// An unrescoped row: scope_canon = '' because no Rescoper ever ran
	// for it. It must stay exactly as unreachable as it already was --
	// the backfill must not touch it.
	if _, err = s.pgdb.Exec(ctx, `
INSERT INTO cortex_memories (agent_id, session_id, kind, content, metadata, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon)
VALUES ($1, '', 'conversation', '{"role":"user","content":"orphaned"}', '{}', '', '', '', '{}', '')`,
		agentID.String(),
	); err != nil {
		t.Fatalf("insert unrescoped legacy row: %v", err)
	}

	// Roll back the bookkeeping row for the backfill migration only, so
	// the orchestrator treats it as pending again -- the initial Migrate
	// call above already ran it once, against an empty database, which
	// is exactly the "proves nothing" shape this test exists to avoid.
	if _, err = s.pgdb.Exec(ctx,
		`DELETE FROM grove_migrations WHERE version = $1 AND "group" = 'cortex'`,
		"20260824000004"); err != nil {
		t.Fatalf("simulate unrecorded backfill version: %v", err)
	}

	// A Rescoper is required on this call: the unrescoped row above would
	// otherwise fail Migrate outright (rescopeLegacyRows refuses to guess
	// at a scope for it), before the backfill migration even gets a
	// chance to run. stubRescoper's answer is never actually read for
	// this row's content -- what matters below is that it lands AFTER
	// the backfill migration's own Up already completed within this same
	// Migrate() call, so it remains ineligible for the backfill even
	// though it now carries a real scope.
	rescoper := stubRescoper{fn: func(string, string) (cortex.Scope, error) {
		return cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_rescoped"}}}, nil
	}}
	if err = s.Migrate(ctx, cortex.WithRescoper(rescoper)); err != nil {
		t.Fatalf("re-migrate to run backfill: %v", err)
	}

	sessCtx := cortex.WithScope(ctx, scope)
	sessions, err := s.ListSessions(sessCtx, &session.ListFilter{AgentID: agentID, DefaultOnly: true})
	if err != nil {
		t.Fatalf("list sessions after backfill: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions after backfill = %d, want exactly 1 default session", len(sessions))
	}
	got := sessions[0]
	if !got.IsDefault {
		t.Errorf("backfilled session IsDefault = false, want true")
	}
	if got.Title != "Default" {
		t.Errorf("backfilled session Title = %q, want %q", got.Title, "Default")
	}
	if got.MessageCount != 2 {
		t.Errorf("backfilled session MessageCount = %d, want 2 (distinct role+content pairs, not the 3 physical rows)", got.MessageCount)
	}
	if got.LastMessage != "hello" {
		// legacyRows' chronological order (created_at ASC, matching
		// insertion order): hello, hi there, hello again -- the last
		// non-empty-content message is the resaved "hello".
		t.Errorf("backfilled session LastMessage = %q, want %q", got.LastMessage, "hello")
	}

	// No row is deleted or merged: LoadConversation must still return all
	// 3 physical rows, duplicate included. message_count reads right
	// without destroying any data -- the whole point of counting
	// distinct pairs instead of deleting rows to make a raw count match.
	msgs, err := s.LoadConversation(sessCtx, agentID, got.ID, 0)
	if err != nil {
		t.Fatalf("load backfilled conversation: %v", err)
	}
	if len(msgs) != 3 {
		t.Errorf("LoadConversation after backfill returned %d messages, want 3 (physical rows are never touched)", len(msgs))
	}

	// The formerly-unrescoped row: rescopeLegacyRows runs AFTER the
	// migration group in the same Migrate() call, so by the time it
	// assigns this row a real scope_canon, migration 20260824000004 has
	// already finished its one and only Up for this grove-recorded
	// version. The row is therefore rescoped (reachable to every OTHER
	// scoped query now) but still has session_id = '' -- exactly the
	// documented limitation: a row that gets its scope in the same
	// Migrate() call the backfill runs in is not retroactively backfilled
	// into a session.
	var (
		orphanedScopeCanon string
		orphanedSessionID  string
	)
	row := s.pgdb.QueryRow(ctx, `SELECT scope_canon, session_id FROM cortex_memories WHERE content = '{"role":"user","content":"orphaned"}'`)
	if err := row.Scan(&orphanedScopeCanon, &orphanedSessionID); err != nil {
		t.Fatalf("read formerly-unrescoped row: %v", err)
	}
	if orphanedScopeCanon == "" {
		t.Errorf("formerly-unrescoped row scope_canon still empty, want rescopeLegacyRows to have assigned one")
	}
	if orphanedSessionID != "" {
		t.Errorf("formerly-unrescoped row got session_id = %q, want unchanged empty string (backfill already ran before rescoping this row)", orphanedSessionID)
	}
}

// stubRescoper is a minimal cortex.Rescoper for tests that need Migrate to
// accept unscoped legacy rows without erroring.
type stubRescoper struct {
	fn func(appID, tenantID string) (cortex.Scope, error)
}

func (r stubRescoper) Rescope(_ context.Context, appID, tenantID string) (cortex.Scope, error) {
	return r.fn(appID, tenantID)
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
