package postgres

import (
	"context"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/suspension"
)

func testScopeInventory(t *testing.T, s *Store) {
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "inventory"}}})
	ag := &agent.Config{ID: id.NewAgentID(), Name: "legacy-overflow"}
	if err := s.Create(ctx, ag); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pgdb.Exec(ctx, `UPDATE cortex_agents SET scope_extra = '{"region":"hidden"}'::jsonb WHERE id = $1`, ag.ID.String()); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(ctx, ag.ID); err == nil || got != nil {
		t.Fatal("unsafe row was returned")
	}
	if err := s.Update(ctx, ag); err == nil {
		t.Fatal("unsafe row was writable")
	}
	var issues []store.ScopeIssue
	if err := s.InspectScopes(ctx, func(issue store.ScopeIssue) error { issues = append(issues, issue); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].ID != ag.ID.String() || issues[0].Resource != "cortex_agents" {
		t.Fatalf("missing inventory: %+v", issues)
	}
	if got, err := s.Get(ctx, ag.ID); err == nil || got != nil {
		t.Fatal("inventory rewrote unsafe identity")
	}
	r := &run.Run{ID: id.NewAgentRunID(), AgentID: ag.ID, State: run.StatePaused}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	susp := &suspension.Suspension{ID: id.NewSuspensionID(), RunID: r.ID, Reason: suspension.ReasonExternalTool}
	if err := s.CreateSuspension(ctx, susp); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pgdb.Exec(ctx, `UPDATE cortex_suspensions SET scope_extra = '{"region":"hidden"}'::jsonb WHERE id = $1`, susp.ID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimSuspension(ctx, r.ID); err == nil {
		t.Fatal("unsafe suspension claimed")
	}
	got, err := s.GetRun(ctx, r.ID)
	if err != nil || got.State != run.StatePaused {
		t.Fatalf("failed claim changed run: %v %v", got, err)
	}
}
