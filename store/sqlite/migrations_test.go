package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate"
	"github.com/xraph/grove/migrate"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/session"
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

// TestMigrate_backfillDefaultSessions is sqlite's counterpart to the
// identical postgres test: it proves the default-session backfill
// against a genuinely pre-v1.9.0 shape built with direct SQL, bypassing
// SaveConversation (which always stamps a real session_id today), and
// proves the fix for the skipped-version upgrade bug a review round
// caught -- rows starting at scope_canon = ”, a Rescoper supplied, ONE
// Migrate() call, and the conversation reachable through a default
// session by the time it returns. See the postgres test for the full
// reasoning; this is the same scenario against sqlite's `?` placeholder
// style.
func TestMigrate_backfillDefaultSessions(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "cortex_backfill_test.db")

	drv := sqlitedriver.New()
	if err := drv.Open(ctx, dsn); err != nil {
		t.Fatalf("open sqlite driver: %v", err)
	}
	db, err := grove.Open(drv)
	if err != nil {
		t.Fatalf("grove open: %v", err)
	}
	s := New(db)
	t.Cleanup(func() { _ = s.Close() })

	// Schema-only: an empty database has no unscoped rows, so this needs
	// no Rescoper. Tables have to exist before the raw SQL inserts below
	// can write to them.
	if err = s.Migrate(ctx); err != nil {
		t.Fatalf("initial migrate: %v", err)
	}

	agentID := id.NewAgentID()
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_legacy"}}}
	canon := scope.Canonical()

	// Group 1: already scoped, not yet sessioned. Two logically distinct
	// messages, the first duplicated once with a different Timestamp --
	// the shape engine.llmToMemory's reload-then-resave round trip
	// produced pre-daa7e44.
	legacyRows := []string{
		`{"role":"user","content":"hello","timestamp":"2024-01-01T00:00:00Z"}`,
		`{"role":"assistant","content":"hi there","timestamp":"2024-01-01T00:00:01Z"}`,
		`{"role":"user","content":"hello","timestamp":"2024-01-02T00:00:00Z"}`, // duplicate resave
	}
	for _, content := range legacyRows {
		if _, err = s.sdb.Exec(ctx, `
INSERT INTO cortex_memories (agent_id, session_id, kind, content, metadata, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon)
VALUES (?, '', 'conversation', ?, '{}', ?, '', '', '{}', ?)`,
			agentID.String(), content, canon, canon,
		); err != nil {
			t.Fatalf("insert legacy conversation row: %v", err)
		}
	}

	// Group 2: the skipped-version shape -- scope_canon = '' because no
	// migration and no Rescoper has ever touched it, exactly what a
	// genuinely pre-v1.8.0 row looks like the moment before an upgrade
	// that jumps straight to v1.9.0 in one Migrate() call.
	skippedVersionAgent := id.NewAgentID()
	if _, err = s.sdb.Exec(ctx, `
INSERT INTO cortex_memories (agent_id, session_id, kind, content, metadata, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon)
VALUES (?, '', 'conversation', '{"role":"user","content":"skipped-version"}', '{}', '', '', '', '{}', '')`,
		skippedVersionAgent.String(),
	); err != nil {
		t.Fatalf("insert skipped-version legacy row: %v", err)
	}

	// The single call under test: a Rescoper is supplied (required,
	// since the skipped-version row above would otherwise fail Migrate
	// outright), and rescoping AND backfilling both groups must happen
	// inside this one Migrate() call.
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
		t.Errorf("backfilled session LastMessage = %q, want %q", got.LastMessage, "hello")
	}

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
	// must have picked it up.
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
}

// stubRescoper is a minimal cortex.Rescoper for tests that need Migrate to
// accept unscoped legacy rows without erroring.
type stubRescoper struct {
	fn func(appID, tenantID string) (cortex.Scope, error)
}

func (r stubRescoper) Rescope(_ context.Context, appID, tenantID string) (cortex.Scope, error) {
	return r.fn(appID, tenantID)
}
