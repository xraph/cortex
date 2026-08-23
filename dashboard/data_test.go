package dashboard

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/session"
	"github.com/xraph/cortex/store/sqlite"
)

// newSQLiteStoreForTest opens a migrated sqlite store backed by a
// temp-file database, mirroring engine/session_test.go's helper of the
// same name -- this needs a real backend, not a fake, because the bug
// under test lives in scopePredicates' Exact handling inside the store.
func newSQLiteStoreForTest(t *testing.T) *sqlite.Store {
	t.Helper()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "cortex_dashboard_test.db")
	drv := sqlitedriver.New()
	if err := drv.Open(ctx, dsn); err != nil {
		t.Fatalf("open sqlite driver: %v", err)
	}
	db, err := grove.Open(drv)
	if err != nil {
		t.Fatalf("grove open: %v", err)
	}
	s := sqlite.New(db)
	if migrateErr := s.Migrate(ctx); migrateErr != nil {
		t.Fatalf("migrate: %v", migrateErr)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestFetchDefaultSessionID_DoesNotMatchDescendantScope is the dashboard
// counterpart to engine/session_test.go's
// TestResolveSession_DoesNotMatchDescendantScope: fetchDefaultSessionID
// (dashboard/data.go) left Exact unset on its ListSessions(DefaultOnly)
// call, the same gap fixed at the three write-path call sites, but
// reached here through renderMemory/renderChat's read path instead.
//
// scopePredicates(scope, false) skips empty levels, so a workspace-only
// viewer's lookup matched a descendant project's default session too.
// That session id then went into LoadConversation under the viewer's
// own shallow context; LoadConversation's own scope predicate is ALSO
// non-exact, so a workspace-only context has no project-level predicate
// to reject the deeper-scoped rows -- they matched, and a parent-scope
// dashboard viewer was served a descendant project's conversation in
// both the Memory and Chat tabs.
//
// This proves a viewer at {workspace=ws_x} gets NOTHING back through
// this exact fetchDefaultSessionID -> LoadConversation sequence, even
// though a real conversation exists for the same agent one level
// deeper, at {workspace=ws_x, project=p1}.
func TestFetchDefaultSessionID_DoesNotMatchDescendantScope(t *testing.T) {
	s := newSQLiteStoreForTest(t)
	ctx := context.Background()

	agentID := id.NewAgentID()

	deepScope := cortex.Scope{Levels: []cortex.Level{
		{Key: "workspace", Value: "ws_x"},
		{Key: "project", Value: "p1"},
	}}
	deepCtx := cortex.WithScope(ctx, deepScope)

	deepSession := &session.Session{ID: id.NewSessionID(), AgentID: agentID, IsDefault: true, Title: "Default"}
	if err := s.CreateSession(deepCtx, deepSession); err != nil {
		t.Fatalf("create deep default session: %v", err)
	}
	if err := s.SaveConversation(deepCtx, agentID, deepSession.ID, []memory.Message{
		{Role: "user", Content: "deep scope's private conversation"},
	}); err != nil {
		t.Fatalf("save deep conversation: %v", err)
	}

	// The viewer's own scope: shallower than the conversation above,
	// and never given a default session of its own.
	shallowScope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}}
	shallowCtx := cortex.WithScope(ctx, shallowScope)

	sessionID := fetchDefaultSessionID(shallowCtx, s, agentID)
	if sessionID == deepSession.ID {
		t.Fatalf("fetchDefaultSessionID at the shallow scope returned the DEEPER scope's session (%s); it must not match a descendant scope", deepSession.ID)
	}

	messages, err := s.LoadConversation(shallowCtx, agentID, sessionID, 100)
	if err != nil {
		t.Fatalf("load conversation at shallow scope: %v", err)
	}
	if len(messages) != 0 {
		t.Errorf("LoadConversation for a shallow-scope dashboard viewer returned %d message(s), want 0 -- got %v (the descendant scope's conversation leaked to a parent-scope viewer)", len(messages), messages)
	}
}
