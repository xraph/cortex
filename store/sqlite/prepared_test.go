package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

func TestPreparedPauseRejectsPreviousClaimPredicate(t *testing.T) {
	s := newTestStore(t)
	ctx := cortex.WithScope(context.Background(), scopeOf("prepared"))
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker"}
	if err := s.Create(ctx, ag); err != nil {
		t.Fatal(err)
	}
	r := &run.Run{ID: id.NewAgentRunID(), AgentID: ag.ID, State: run.StatePaused, Provenance: run.ExecutionProvenance{PreparedDigest: "snapshot"}}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(-time.Second)
	susp := &suspension.Suspension{ID: id.NewSuspensionID(), RunID: r.ID, Reason: suspension.ReasonExternalTool, ExpiresAt: &expires}
	if err := s.CreateSuspension(ctx, susp); err != nil {
		t.Fatal(err)
	}
	// Both previous normal and expired claims require this literal state.
	// Omitting their additional scope/deadline predicates makes the probe
	// stronger: even the broader legacy update cannot take this run.
	res, err := s.sdb.Exec(ctx, "UPDATE cortex_runs SET state = 'running' WHERE id = ? AND state = 'paused'", r.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("previous worker claimed a prepared run")
	}
	got, err := s.GetRun(ctx, r.ID)
	if err != nil || got.State != run.StatePaused {
		t.Fatalf("new reader lost paused state: %+v %v", got, err)
	}
	if _, err = s.ClaimExpiredSuspension(ctx, r.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
}
