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

// TestMigrate_backfillDefaultSessions proves the default-session backfill
// against a genuinely pre-v1.9.0 shape, not an empty database: rows are
// inserted directly with SQL, bypassing SaveConversation entirely (which
// always stamps a real session_id today and so could never produce the
// shape this backfill exists to repair).
//
// This also proves the fix for the skipped-version upgrade bug a review
// round caught: a host jumping straight from pre-v1.8.0 to v1.9.0 in one
// Migrate() call has every legacy conversation row sitting at
// scope_canon = "" at the point the migration group used to run its
// one-shot backfill, before rescopeLegacyRows had a chance to assign a
// real scope in that same call -- so the old, migration-Up-based
// backfill found nothing, and because grove never retries a recorded
// migration version, no later boot found anything either. Those rows
// would end up scoped (reachable to every other scoped query, once
// rescopeLegacyRows got to them) but permanently orphaned from a
// session. backfillDefaultSessions now runs directly from Store.Migrate,
// AFTER rescopeLegacyRows, unconditionally on every boot -- so this test
// exercises exactly that: rows starting at scope_canon = "", a Rescoper
// supplied, ONE Migrate() call, and the conversation reachable through a
// default session by the time it returns.
//
// Two groups of legacy rows exercise this. The first (three rows, one
// (agent_id, scope) pair, already carrying a real scope_canon) is the
// "ordinary" post-v1.8.0-pre-v1.9.0 shape and also covers the
// message_count dedup: two of the three rows simulate the pre-daa7e44
// duplication bug: the reasoning loop used to re-save a run's entire
// reloaded history on every turn, so the same logical message can
// appear as more than one physical row with a different Timestamp
// field but identical Role/Content -- see
// backfillDefaultSessions' comment in migrations.go for the full
// reasoning behind counting message_count as distinct (role, content)
// pairs instead of a raw row count. The second group (one row, scope_l0
// and scope_canon both "") is the skipped-version shape: it starts with
// no scope at all, so rescopeLegacyRows -- not this test -- is what
// gives it one, inside the same Migrate() call the backfill runs in.
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
	// Schema-only: an empty database has no unscoped rows, so this needs
	// no Rescoper and both rescopeLegacyRows and backfillDefaultSessions
	// are no-ops here. Tables have to exist before the raw SQL inserts
	// below can write to them.
	if err = s.Migrate(ctx); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}

	agentID := id.NewAgentID()
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_legacy"}}}
	canon := scope.Canonical() // "workspace=ws_legacy"

	// Group 1: already scoped, not yet sessioned. Two logically distinct
	// messages, the first duplicated once with a different Timestamp --
	// exactly the shape engine.llmToMemory's reload-then-resave round
	// trip produced pre-daa7e44. Raw content-string equality would see
	// three different rows here; the backfill's Role+Content dedup must
	// still land on 2.
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

	// Group 2: the skipped-version shape. scope_canon = '' here because
	// no migration and no Rescoper has ever touched it -- this is what a
	// genuinely pre-v1.8.0 row looks like the moment before an upgrade
	// that jumps straight to v1.9.0 in one Migrate() call.
	skippedVersionAgent := id.NewAgentID()
	if _, err = s.pgdb.Exec(ctx, `
INSERT INTO cortex_memories (agent_id, session_id, kind, content, metadata, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon)
VALUES ($1, '', 'conversation', '{"role":"user","content":"skipped-version"}', '{}', '', '', '', '{}', '')`,
		skippedVersionAgent.String(),
	); err != nil {
		t.Fatalf("insert skipped-version legacy row: %v", err)
	}

	// The single call under test: a Rescoper is supplied (required,
	// since rescopeLegacyRows refuses to guess a scope for the
	// skipped-version row above), and everything -- rescoping AND
	// backfilling both groups -- has to happen inside this one
	// Migrate() call.
	rescopedScope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_rescoped"}}}
	rescoper := stubRescoper{fn: func(string, string) (cortex.Scope, error) {
		return rescopedScope, nil
	}}
	if err = s.Migrate(ctx, cortex.WithRescoper(rescoper)); err != nil {
		t.Fatalf("migrate with rescoper: %v", err)
	}

	// Group 1: the ordinary case, plus the message_count dedup proof.
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

	// Group 2: the skipped-version proof. rescopeLegacyRows assigned
	// rescopedScope to this row inside the SAME Migrate() call, and
	// backfillDefaultSessions -- running after it, in that same call --
	// must have picked it up: a default session must exist for it, and
	// the row itself must be reachable through LoadConversation. Before
	// the fix, this row would have stayed at session_id = '' forever:
	// backfillDefaultSessions ran as the migration's one-shot Up, before
	// rescopeLegacyRows in that same call ever touched it, so its filter
	// (scope_canon != '') never matched, and grove would not have
	// retried Up on any later boot even after the row had a real scope.
	rescopedCtx := cortex.WithScope(ctx, rescopedScope)
	rescopedSessions, err := s.ListSessions(rescopedCtx, &session.ListFilter{AgentID: skippedVersionAgent, DefaultOnly: true})
	if err != nil {
		t.Fatalf("list sessions for skipped-version agent: %v", err)
	}
	if len(rescopedSessions) != 1 {
		t.Fatalf("sessions for skipped-version agent = %d, want exactly 1 default session (rescope + backfill must both land in one Migrate() call)", len(rescopedSessions))
	}
	rescopedSession := rescopedSessions[0]
	if rescopedSession.MessageCount != 1 {
		t.Errorf("skipped-version session MessageCount = %d, want 1", rescopedSession.MessageCount)
	}
	if rescopedSession.LastMessage != "skipped-version" {
		t.Errorf("skipped-version session LastMessage = %q, want %q", rescopedSession.LastMessage, "skipped-version")
	}

	rescopedMsgs, err := s.LoadConversation(rescopedCtx, skippedVersionAgent, rescopedSession.ID, 0)
	if err != nil {
		t.Fatalf("load skipped-version conversation: %v", err)
	}
	if len(rescopedMsgs) != 1 || rescopedMsgs[0].Content != "skipped-version" {
		t.Fatalf("LoadConversation for skipped-version agent = %v, want exactly one message with content %q", rescopedMsgs, "skipped-version")
	}

	// unbackfillDefaultSessions (Down) regression, fix round 2's Finding
	// 4: the marker it depends on to find its own rows used to live in
	// Metadata, the column session.Session documents as belonging to
	// the host. Simulate a host PUTting its own metadata over the
	// group-1 session -- exactly what would have silently destroyed the
	// old marker -- and prove Down still finds and removes it, because
	// backfilled_by is a separate, cortex-owned column UpdateSession
	// never carries in its mutable-column list.
	got.Metadata = map[string]any{"host_field": "host_value"}
	if err = s.UpdateSession(sessCtx, got); err != nil {
		t.Fatalf("host update of session metadata: %v", err)
	}

	// An organic default session, created the ordinary way, that Down
	// must leave alone.
	organicAgent := id.NewAgentID()
	organicScope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_organic"}}}
	organicCtx := cortex.WithScope(ctx, organicScope)
	organicSession := &session.Session{ID: id.NewSessionID(), AgentID: organicAgent, IsDefault: true, Title: "Default"}
	if err = s.CreateSession(organicCtx, organicSession); err != nil {
		t.Fatalf("create organic session: %v", err)
	}

	executor := &pgMigrateExecutor{pgdb: s.pgdb}
	if err = unbackfillDefaultSessions(ctx, executor); err != nil {
		t.Fatalf("unbackfillDefaultSessions: %v", err)
	}

	afterDown, err := s.ListSessions(sessCtx, &session.ListFilter{AgentID: agentID, DefaultOnly: true})
	if err != nil {
		t.Fatalf("list sessions after Down: %v", err)
	}
	if len(afterDown) != 0 {
		t.Errorf("sessions for group 1's agent after Down = %d, want 0 (Down must remove the session it created, even after a host overwrote its metadata)", len(afterDown))
	}

	var resetSessionID string
	if scanErr := s.pgdb.QueryRow(ctx,
		`SELECT session_id FROM cortex_memories WHERE agent_id = $1 AND kind = 'conversation' LIMIT 1`,
		agentID.String(),
	).Scan(&resetSessionID); scanErr != nil {
		t.Fatalf("read conversation row session_id after Down: %v", scanErr)
	}
	if resetSessionID != "" {
		t.Errorf("conversation row session_id after Down = %q, want empty (Down must reset rows it pointed at the removed session)", resetSessionID)
	}

	stillThere, err := s.GetSession(organicCtx, organicSession.ID)
	if err != nil {
		t.Fatalf("get organic session after Down: %v", err)
	}
	if stillThere.ID != organicSession.ID {
		t.Errorf("organic session after Down = %+v, want it untouched (Down must only remove rows it created itself)", stillThere)
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
