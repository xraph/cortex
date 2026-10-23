package sqlite

import (
	"context"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

func TestRunOutcomeMigrationPreservesHistoricalCompletion(t *testing.T) {
	s := newTestStore(t)
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "migration"}}})
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
	for _, column := range []string{"stop_reason", "finish_reason", "verification_status", "result_ref", "provenance", "usage", "budget", "revision"} {
		if _, err := s.sdb.Exec(ctx, "ALTER TABLE cortex_runs DROP COLUMN "+column); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, err := s.sdb.Exec(ctx, `DELETE FROM grove_migrations WHERE version IN (?, ?, ?) AND "group" = 'cortex'`, "20261018000001", "20261018000002", "20261023000001"); err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Revision != 0 || got.Provenance.PreparedDigest != "" || got.Budget.MaxTokens != 0 || got.Usage.Tokens != 0 || got.State != run.StateCompleted || got.Output != "old answer" || got.StopReason != "" || got.VerificationStatus != verification.StatusUnchecked {
			t.Fatalf("migration fabricated assurance: %+v", got)
		}
	}
	historical, loadErr := s.GetRun(ctx, r.ID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if updateErr := s.UpdateRun(ctx, historical); updateErr != nil {
		t.Fatal(updateErr)
	}
	if historical.Revision != 1 {
		t.Fatalf("historical update revision = %d", historical.Revision)
	}
}
