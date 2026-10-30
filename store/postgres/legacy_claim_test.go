package postgres

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

// The two claim UPDATEs v1.9.0 sends (`git show v1.9.0:store/postgres/suspension.go`,
// ClaimSuspension and ClaimExpiredSuspension) for a scope of one level.
// v1.9.0's query builder binds the two paused states as parameters; the
// text here writes them inline as 'paused' and 'paused_prepared'. The
// predicate is the same. They are frozen here as text so a later change to
// this package's builders cannot quietly change what the old worker is
// proven to do. During a rolling upgrade a v1.9.0 replica shares the
// database, and its claim must not take a run this version made ready or
// reclaimable: it would run it a second time.
const (
	legacyClaimSQL        = `UPDATE "cortex_runs" SET state = $1, revision = revision + 1, updated_at = $2 WHERE revision >= 0 AND revision < $3 AND id = $4 AND state IN ('paused', 'paused_prepared') AND EXISTS (SELECT 1 FROM cortex_suspensions cs WHERE cs.run_id = $5 AND (cs.expires_at IS NULL OR cs.expires_at > $6) AND cs.scope_l0 = $7 AND cs.scope_l1 = $8 AND cs.scope_l2 = $9 AND cs.scope_canon = $10 AND cs.scope_extra = $11) AND scope_l0 = $12 AND scope_l1 = $13 AND scope_l2 = $14 AND scope_canon = $15 AND scope_extra = $16`
	legacyExpiredClaimSQL = `UPDATE "cortex_runs" SET state = $1, revision = revision + 1, updated_at = $2 WHERE revision >= 0 AND revision < $3 AND id = $4 AND state IN ('paused', 'paused_prepared') AND EXISTS (SELECT 1 FROM cortex_suspensions cs WHERE cs.run_id = $5 AND cs.expires_at IS NOT NULL AND cs.expires_at <= $6 AND cs.scope_l0 = $7 AND cs.scope_l1 = $8 AND cs.scope_l2 = $9 AND cs.scope_canon = $10 AND cs.scope_extra = $11) AND scope_l0 = $12 AND scope_l1 = $13 AND scope_l2 = $14 AND scope_canon = $15 AND scope_extra = $16`
)

func TestLegacyClaimPredicateSkipsNewStates(t *testing.T) {
	ctx, dsn := startMigratePostgres(t)
	s := New(openMigrateDB(ctx, t, dsn))
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "legacy"}}}
	ctx = cortex.WithScope(ctx, scope)
	agentID := id.NewAgentID()
	if err := s.Create(ctx, &agent.Config{ID: agentID, Name: "legacy-claim"}); err != nil {
		t.Fatalf("create agent: %v", err)
	}

	paused := func() (*run.Run, *suspension.Suspension) {
		t.Helper()
		r := &run.Run{ID: id.NewAgentRunID(), AgentID: agentID, State: run.StatePaused, Input: "input"}
		if err := s.CreateRun(ctx, r); err != nil {
			t.Fatalf("create paused run: %v", err)
		}
		susp := &suspension.Suspension{ID: id.NewSuspensionID(), RunID: r.ID, Reason: suspension.ReasonApproval}
		if err := s.CreateSuspension(ctx, susp); err != nil {
			t.Fatalf("create suspension: %v", err)
		}
		return r, susp
	}

	ready, readySusp := paused()
	if _, err := s.MarkReady(ctx, ready.ID, run.Readiness{Source: run.ReadyApproval, At: time.Now().UTC(), SuspensionID: readySusp.ID}); err != nil {
		t.Fatalf("MarkReady: %v", err)
	}
	reclaimable := &run.Run{ID: id.NewAgentRunID(), AgentID: agentID, State: run.StateRunning, Input: "input", Lease: &run.Lease{Owner: "w1", ExpiresAt: time.Now().UTC().Add(-time.Minute)}}
	if err := s.CreateRun(ctx, reclaimable); err != nil {
		t.Fatalf("create running run: %v", err)
	}
	// A suspension row for the reclaimable run as well, so that only the
	// state predicate stands between the old claim and the row.
	reclaimableSusp := &suspension.Suspension{ID: id.NewSuspensionID(), RunID: reclaimable.ID, Reason: suspension.ReasonApproval}
	if err := s.CreateSuspension(ctx, reclaimableSusp); err != nil {
		t.Fatalf("create suspension: %v", err)
	}
	if _, err := s.MarkReclaimable(ctx, reclaimable.ID, time.Now().UTC(), ""); err != nil {
		t.Fatalf("MarkReclaimable: %v", err)
	}

	preds := scopePredicates(scope, true)
	exec := func(query string, runID id.AgentRunID, now time.Time) int64 {
		t.Helper()
		args := make([]any, 0, 6+2*len(preds))
		args = append(args, string(run.StateRunning), now, int64(math.MaxInt64), runID.String(), runID.String(), now)
		for range 2 {
			for _, p := range preds {
				args = append(args, p.Value)
			}
		}
		res, err := s.pgdb.Exec(ctx, query, args...)
		if err != nil {
			t.Fatalf("legacy claim: %v", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			t.Fatalf("legacy claim rows affected: %v", err)
		}
		return n
	}
	setDeadlines := func(deadline *time.Time) {
		t.Helper()
		if _, err := s.pgdb.Exec(ctx, "UPDATE cortex_suspensions SET expires_at = $1", deadline); err != nil {
			t.Fatalf("set suspension deadlines: %v", err)
		}
	}

	expired := time.Now().UTC().Add(-time.Hour)
	cases := []struct {
		name     string
		query    string
		deadline *time.Time
	}{
		{"ClaimSuspension", legacyClaimSQL, nil},
		{"ClaimExpiredSuspension", legacyExpiredClaimSQL, &expired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			control, _ := paused()
			setDeadlines(tc.deadline)
			now := time.Now().UTC()
			if n := exec(tc.query, ready.ID, now); n != 0 {
				t.Errorf("v1.9.0 %s took a ready run: %d rows", tc.name, n)
			}
			if n := exec(tc.query, reclaimable.ID, now); n != 0 {
				t.Errorf("v1.9.0 %s took a reclaimable run: %d rows", tc.name, n)
			}
			if n := exec(tc.query, control.ID, now); n != 1 {
				t.Fatalf("v1.9.0 %s on a paused run = %d rows, want 1: the frozen SQL no longer matches anything", tc.name, n)
			}
		})
	}

	for _, tc := range []struct {
		r    *run.Run
		susp *suspension.Suspension
	}{{ready, readySusp}, {reclaimable, reclaimableSusp}} {
		current, err := s.GetRun(ctx, tc.r.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		_, err = s.ClaimSuspensionVersion(ctx, suspension.ClaimVersion{RunID: current.ID, SuspensionID: tc.susp.ID, RunRevision: current.Revision})
		if !errors.Is(err, cortex.ErrNotSuspended) {
			t.Errorf("v1.9.0 ClaimSuspensionVersion on a %s run = %v, want ErrNotSuspended", current.State, err)
		}
		after, err := s.GetRun(ctx, tc.r.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if after.State != current.State || after.Revision != current.Revision {
			t.Errorf("legacy claims moved a %s run to %s, revision %d to %d", current.State, after.State, current.Revision, after.Revision)
		}
	}
}
