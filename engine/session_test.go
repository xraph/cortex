package engine_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/session"
	"github.com/xraph/cortex/store/scopespy"
	"github.com/xraph/cortex/store/sqlite"
)

// newEngineWithRealStore builds an engine over a migrated SQLite store
// backed by a temporary file. resolveSession's default-session behavior
// depends on a real unique index enforcing one default per (agent, scope),
// which scopespy's in-memory fakes don't model — this needs the genuine
// backend.
func newEngineWithRealStore(t *testing.T) *engine.Engine {
	t.Helper()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "cortex_test.db")
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

	e, err := engine.New(engine.WithStore(s))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return e
}

// mustCreateSessionViaEngine creates a non-default session directly
// through the engine's store accessor, for tests that need an explicit
// session id to pass as an override.
//
//nolint:revive // ctx placement matches the required test call site; test-only helper, not a public API
func mustCreateSessionViaEngine(t *testing.T, e *engine.Engine, ctx context.Context, agentID id.AgentID, title string) id.SessionID {
	t.Helper()
	s := &session.Session{ID: id.NewSessionID(), AgentID: agentID, Title: title}
	if err := e.Store().CreateSession(ctx, s); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return s.ID
}

// Two runs with no session must land in the SAME default session. If
// resolveSession created one per call, every run would start a fresh
// thread and the agent would lose its history, which is the behavior
// this release exists to preserve for callers that never adopt sessions.
func TestResolveSession_DefaultIsStableAcrossRuns(t *testing.T) {
	e := newEngineWithRealStore(t)
	ctx := cortex.WithScope(context.Background(),
		cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}})
	agentID := id.NewAgentID()

	first, err := engine.ExportResolveSession(e, ctx, agentID, id.SessionID{})
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	second, err := engine.ExportResolveSession(e, ctx, agentID, id.SessionID{})
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if first != second {
		t.Errorf("two default resolutions gave %s and %s; the default must be stable", first, second)
	}
}

func TestResolveSession_OverrideWins(t *testing.T) {
	e := newEngineWithRealStore(t)
	ctx := cortex.WithScope(context.Background(),
		cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}})
	agentID := id.NewAgentID()

	explicit := mustCreateSessionViaEngine(t, e, ctx, agentID, "thread")
	got, err := engine.ExportResolveSession(e, ctx, agentID, explicit)
	if err != nil {
		t.Fatalf("resolve with override: %v", err)
	}
	if got != explicit {
		t.Errorf("resolve returned %s, want the explicit session %s", got, explicit)
	}
}

// TestRunAgent_DefaultSessionReachesActualMessageCount is the end-to-end
// proof for the counter invariant this phase exists to guarantee: after
// runs with no session override, every message the react loop wrote must
// be reachable through the agent's resolved default session, and that
// session's MessageCount must equal the number of message ROWS actually
// under it — never the number of runs, and never the number of
// SaveConversation calls (react.go calls it once per run, and each call's
// batch re-includes the whole reloaded history alongside the new turn, so
// a naive "+1 per call" or "+1 per row this run added" counter would both
// drift from LoadConversation's real count here). This uses a real
// sqlite backend, not scopespy: the spy fakes every store method and so
// can't catch a counter that drifted from the rows it's supposed to
// describe — only a store that actually counts real inserted rows can.
func TestRunAgent_DefaultSessionReachesActualMessageCount(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStoreForTest(t)

	// StaticLLM answers in one step every time, so runReAct's ReAct loop
	// always terminates after saving exactly one assistant message per
	// run (plus whatever history it reloaded), without needing a tool
	// double.
	e, err := engine.New(engine.WithStore(s), engine.WithLLM(scopespy.StaticLLM("ack")))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	scopedCtx := cortex.WithScope(ctx,
		cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_msgcount"}}})

	agentCfg := &agent.Config{ID: id.NewAgentID(), Name: "assistant", MaxSteps: 2}
	if createErr := e.CreateAgent(scopedCtx, agentCfg); createErr != nil {
		t.Fatalf("create agent: %v", createErr)
	}

	if _, runErr := e.RunAgent(scopedCtx, "assistant", "first message", nil); runErr != nil {
		t.Fatalf("first run: %v", runErr)
	}
	if _, runErr := e.RunAgent(scopedCtx, "assistant", "second message", nil); runErr != nil {
		t.Fatalf("second run: %v", runErr)
	}

	sessions, err := s.ListSessions(scopedCtx, &session.ListFilter{AgentID: agentCfg.ID, DefaultOnly: true, Limit: 1})
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("ListSessions(DefaultOnly) = %d session(s), want exactly one default session", len(sessions))
	}
	sess := sessions[0]

	rows, err := s.LoadConversation(scopedCtx, agentCfg.ID, sess.ID, 0)
	if err != nil {
		t.Fatalf("load conversation: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no message rows were saved through the default session; this test proves nothing")
	}

	if sess.MessageCount != len(rows) {
		t.Errorf("session.MessageCount = %d, want %d (the number of message rows actually reachable through this session, not the number of runs or SaveConversation calls)", sess.MessageCount, len(rows))
	}
}

// TestRunAgent_SavesOnlyNewMessagesPerRun is fix round 1's Finding 2 made
// concrete: runReAct/streamReAct used to pass the WHOLE reloaded
// `messages` slice (history included) to SaveConversation on every run,
// not just the turn the run actually added. Three runs in the same
// session would then write 2, then 6, then 14 rows (2*N history-fold
// duplication) instead of 2, 4, 6 -- and worse, LoadConversation orders
// created_at ASC with LIMIT 100 and no offset, so once duplication pushed
// the row count past 100 (around the sixth run) the history the agent
// saw would freeze on an ever-older prefix while new turns kept being
// written but never read back, silently going deaf to the conversation
// it's actually in.
//
// This asserts the row count grows by exactly the new messages each run
// added (2 per run here: one user turn, one assistant reply, no tool
// calls with StaticLLM), never by the whole accumulated history.
func TestRunAgent_SavesOnlyNewMessagesPerRun(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStoreForTest(t)

	e, err := engine.New(engine.WithStore(s), engine.WithLLM(scopespy.StaticLLM("ack")))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	scopedCtx := cortex.WithScope(ctx,
		cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_growth"}}})

	agentCfg := &agent.Config{ID: id.NewAgentID(), Name: "assistant", MaxSteps: 2}
	if createErr := e.CreateAgent(scopedCtx, agentCfg); createErr != nil {
		t.Fatalf("create agent: %v", createErr)
	}

	const messagesPerRun = 2 // one user turn + one assistant reply, no tool calls
	var sessionID id.SessionID
	for i := 1; i <= 3; i++ {
		if _, runErr := e.RunAgent(scopedCtx, "assistant", fmt.Sprintf("message %d", i), nil); runErr != nil {
			t.Fatalf("run %d: %v", i, runErr)
		}

		sessions, listErr := s.ListSessions(scopedCtx, &session.ListFilter{AgentID: agentCfg.ID, DefaultOnly: true, Limit: 1})
		if listErr != nil {
			t.Fatalf("list sessions after run %d: %v", i, listErr)
		}
		if len(sessions) != 1 {
			t.Fatalf("ListSessions(DefaultOnly) after run %d = %d session(s), want exactly one", i, len(sessions))
		}
		sess := sessions[0]
		if sessionID.IsNil() {
			sessionID = sess.ID
		} else if sess.ID != sessionID {
			t.Fatalf("run %d landed in a different default session (%s) than run 1 (%s)", i, sess.ID, sessionID)
		}

		rows, loadErr := s.LoadConversation(scopedCtx, agentCfg.ID, sessionID, 0)
		if loadErr != nil {
			t.Fatalf("load conversation after run %d: %v", i, loadErr)
		}
		wantRows := messagesPerRun * i
		if len(rows) != wantRows {
			t.Errorf("after run %d: LoadConversation returned %d row(s), want %d (%d new per run, not the whole reloaded history re-saved each time)",
				i, len(rows), wantRows, messagesPerRun)
		}
		if sess.MessageCount != len(rows) {
			t.Errorf("after run %d: session.MessageCount = %d, want %d (must match the actual row count)", i, sess.MessageCount, len(rows))
		}
	}
}

// newSQLiteStoreForTest opens a migrated sqlite store backed by a
// temp-file database, for tests that need a real backend rather than
// scopespy's in-memory fakes.
func newSQLiteStoreForTest(t *testing.T) *sqlite.Store {
	t.Helper()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "cortex_test.db")
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
