package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/suspension"
)

func TestUnsafeStoredScopeIsQuarantined(t *testing.T) {
	for _, tc := range []struct{ name, canonical, extra string }{
		{"overflow", "workspace=w", `{"region":"hidden"}`},
		{"mismatch", "workspace=other", `{}`},
		{"deep", "workspace=w/project=p/environment=e/region=r", `{"region":"r"}`},
		{"malformed_extra", "workspace=w", `broken`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "w"}}})
			ag := &agent.Config{ID: id.NewAgentID(), Name: "unsafe"}
			if err := s.Create(ctx, ag); err != nil {
				t.Fatal(err)
			}
			if _, err := s.sdb.Exec(ctx, "UPDATE cortex_agents SET scope_canon = ?, scope_extra = ? WHERE id = ?", tc.canonical, tc.extra, ag.ID.String()); err != nil {
				t.Fatal(err)
			}
			if got, err := s.Get(ctx, ag.ID); err == nil || got != nil {
				t.Fatal("unsafe record returned")
			}
			if err := s.Update(ctx, ag); err == nil {
				t.Fatal("unsafe row was writable")
			}
			var issues []store.ScopeIssue
			if err := s.InspectScopes(ctx, func(issue store.ScopeIssue) error { issues = append(issues, issue); return nil }); err != nil {
				t.Fatal(err)
			}
			if len(issues) != 1 || issues[0].ID != ag.ID.String() || issues[0].Canonical != tc.canonical {
				t.Fatalf("missing inventory: %+v", issues)
			}
			var stored string
			if err := s.sdb.QueryRow(ctx, "SELECT scope_canon FROM cortex_agents WHERE id = ?", ag.ID.String()).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if stored != tc.canonical {
				t.Fatal("inventory changed identity")
			}
		})
	}
}

func TestUnsafeSuspensionCannotClaimRun(t *testing.T) {
	s := newTestStore(t)
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "w"}}})
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker"}
	if err := s.Create(ctx, ag); err != nil {
		t.Fatal(err)
	}
	r := &run.Run{ID: id.NewAgentRunID(), AgentID: ag.ID, State: run.StatePaused}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	susp := &suspension.Suspension{ID: id.NewSuspensionID(), RunID: r.ID, Reason: suspension.ReasonExternalTool}
	if err := s.CreateSuspension(ctx, susp); err != nil {
		t.Fatal(err)
	}
	if _, err := s.sdb.Exec(ctx, `UPDATE cortex_suspensions SET scope_extra = '{"region":"hidden"}' WHERE id = ?`, susp.ID.String()); err != nil {
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

// A progress snapshot is keyed by run_id rather than id. One stored under
// an unsafe scope is reported by the inventory like any other row.
func TestUnsafeProgressScopeIsReported(t *testing.T) {
	s := newTestStore(t)
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "w"}}})
	ag := &agent.Config{ID: id.NewAgentID(), Name: "progress"}
	if err := s.Create(ctx, ag); err != nil {
		t.Fatal(err)
	}
	r := &run.Run{ID: id.NewAgentRunID(), AgentID: ag.ID, State: run.StateRunning, Lease: &run.Lease{Owner: "w1", ExpiresAt: time.Now().Add(time.Minute)}}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveProgress(ctx, &suspension.Progress{RunID: r.ID, Owner: "w1"}); err != nil {
		t.Fatal(err)
	}
	var clean []store.ScopeIssue
	if err := s.InspectScopes(ctx, func(issue store.ScopeIssue) error { clean = append(clean, issue); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(clean) != 0 {
		t.Fatalf("a safe snapshot was reported: %+v", clean)
	}
	if _, err := s.sdb.Exec(ctx, `UPDATE cortex_run_progress SET scope_extra = '{"region":"hidden"}' WHERE run_id = ?`, r.ID.String()); err != nil {
		t.Fatal(err)
	}
	var issues []store.ScopeIssue
	if err := s.InspectScopes(ctx, func(issue store.ScopeIssue) error { issues = append(issues, issue); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].Resource != "cortex_run_progress" || issues[0].ID != r.ID.String() {
		t.Fatalf("unsafe snapshot inventory = %+v, want one cortex_run_progress issue for %s", issues, r.ID)
	}
}
