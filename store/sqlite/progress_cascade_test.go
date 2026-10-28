package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

// SQLite enforces a foreign key only on a connection that turned
// foreign_keys on. A run deleted over any other connection must still take
// its progress snapshot with it.
func TestRunProgressCascadesWithoutForeignKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cortex_test.db")
	s := openTestStore(t, path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}})
	agentID := id.NewAgentID()
	if err := s.Create(ctx, &agent.Config{ID: agentID, Name: "cascade"}); err != nil {
		t.Fatal(err)
	}
	r := &run.Run{ID: id.NewAgentRunID(), AgentID: agentID, State: run.StateRunning, Input: "input", Lease: &run.Lease{Owner: "w1", ExpiresAt: time.Now().Add(time.Minute).UTC()}}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProgress(ctx, &suspension.Progress{RunID: r.ID, Owner: "w1", Cont: suspension.Continuation{SystemPrompt: "x"}}); err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}

	plain, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plain.Close() }()
	conn, err := plain.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	var enforced int
	if err = conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enforced); err != nil {
		t.Fatal(err)
	}
	if enforced != 0 {
		t.Fatalf("foreign_keys = %d on the plain connection, want 0 for this test to mean anything", enforced)
	}
	if _, err = conn.ExecContext(ctx, "DELETE FROM cortex_runs WHERE id = ?", r.ID.String()); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	if _, err = s.GetProgress(ctx, r.ID); !errors.Is(err, cortex.ErrProgressNotFound) {
		t.Fatalf("GetProgress after deleting the run without foreign keys = %v, want ErrProgressNotFound", err)
	}
}
