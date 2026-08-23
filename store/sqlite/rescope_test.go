package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

type fixedRescoper struct{ level string }

func (f fixedRescoper) Rescope(_ context.Context, appID, _ string) (cortex.Scope, error) {
	return cortex.Scope{Levels: []cortex.Level{{Key: f.level, Value: appID}}}, nil
}

// tenantRescoper keys off tenantID rather than appID, for tables like
// cortex_runs that only ever carry the former.
type tenantRescoper struct{ level string }

func (r tenantRescoper) Rescope(_ context.Context, _, tenantID string) (cortex.Scope, error) {
	return cortex.Scope{Levels: []cortex.Level{{Key: r.level, Value: tenantID}}}, nil
}

type failingRescoper struct{}

func (failingRescoper) Rescope(_ context.Context, _, _ string) (cortex.Scope, error) {
	return cortex.Scope{}, errors.New("boom")
}

type stubScopeRescoper struct{ scope cortex.Scope }

func (s stubScopeRescoper) Rescope(_ context.Context, _, _ string) (cortex.Scope, error) {
	return s.scope, nil
}

// insertLegacyAgent writes a row with EMPTY scope columns, mimicking a row
// created before v1.8.0. It bypasses the store's Create because Create
// stamps the context scope unconditionally, which would defeat the whole
// point of exercising the pre-scope state. Every test in this file only
// ever needs one agent name, so it's fixed rather than threaded through.
func insertLegacyAgent(t *testing.T, s *Store, agentID, appID string) {
	t.Helper()
	const q = `INSERT INTO cortex_agents
	    (id, name, app_id, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon, enabled)
	    VALUES (?, 'assistant', ?, '', '', '', '{}', '', 1)`
	if _, err := s.sdb.Exec(context.Background(), q, agentID, appID); err != nil {
		t.Fatalf("insert legacy agent: %v", err)
	}
}

// insertLegacyRun writes an unscoped cortex_runs row directly, the same
// way insertLegacyAgent does for agents.
func insertLegacyRun(t *testing.T, s *Store, runID, agentID, tenantID string) {
	t.Helper()
	const q = `INSERT INTO cortex_runs
	    (id, agent_id, tenant_id, state, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon)
	    VALUES (?, ?, ?, 'created', '', '', '', '{}', '')`
	if _, err := s.sdb.Exec(context.Background(), q, runID, agentID, tenantID); err != nil {
		t.Fatalf("insert legacy run: %v", err)
	}
}

// insertScopedRun writes a cortex_runs row that is already scoped, as if
// an earlier rescope pass (or a post-v1.8.0 write) had already handled it.
func insertScopedRun(t *testing.T, s *Store, runID, agentID string, scope cortex.Scope) {
	t.Helper()
	l0, l1, l2, extra := scopeColumns(scope)
	const q = `INSERT INTO cortex_runs
	    (id, agent_id, tenant_id, state, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon)
	    VALUES (?, ?, '', 'created', ?, ?, ?, ?, ?)`
	if _, err := s.sdb.Exec(context.Background(), q, runID, agentID, l0, l1, l2, extra, scope.Canonical()); err != nil {
		t.Fatalf("insert scoped run: %v", err)
	}
}

// insertLegacyStep writes an unscoped cortex_steps row. Steps carry no
// legacy identifier of their own -- only a run_id -- so rescoping one
// depends entirely on its parent run resolving to a scope.
func insertLegacyStep(t *testing.T, s *Store, stepID, runID string) {
	t.Helper()
	const q = `INSERT INTO cortex_steps
	    (id, run_id, "index", scope_l0, scope_l1, scope_l2, scope_extra, scope_canon)
	    VALUES (?, ?, 0, '', '', '', '{}', '')`
	if _, err := s.sdb.Exec(context.Background(), q, stepID, runID); err != nil {
		t.Fatalf("insert legacy step: %v", err)
	}
}

// insertLegacyToolCall writes an unscoped cortex_tool_calls row, which
// like cortex_steps inherits its scope from run_id rather than carrying
// its own legacy identifier.
func insertLegacyToolCall(t *testing.T, s *Store, toolCallID, stepID, runID string) {
	t.Helper()
	const q = `INSERT INTO cortex_tool_calls
	    (id, step_id, run_id, tool_name, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon)
	    VALUES (?, ?, ?, 'search', '', '', '', '{}', '')`
	if _, err := s.sdb.Exec(context.Background(), q, toolCallID, stepID, runID); err != nil {
		t.Fatalf("insert legacy tool call: %v", err)
	}
}

// legacyScopeCanon reads scope_canon straight from the table, so tests can
// verify the write path without depending on a scope-filtered read.
func legacyScopeCanon(t *testing.T, s *Store, table, id string) string {
	t.Helper()
	var canon string
	err := s.sdb.QueryRow(context.Background(),
		`SELECT scope_canon FROM `+table+` WHERE id = ?`, id).Scan(&canon)
	if err != nil {
		t.Fatalf("read back scope_canon from %s: %v", table, err)
	}
	return canon
}

func TestRescope_NoUnscopedRowsNeedsNoRescoper(t *testing.T) {
	s := newTestStore(t)
	if err := s.rescopeLegacyRows(context.Background(), cortex.MigrateOptions{}); err != nil {
		t.Fatalf("a clean database must not require a rescoper: %v", err)
	}
}

func TestRescope_UnscopedRowsWithoutRescoperAborts(t *testing.T) {
	s := newTestStore(t)
	insertLegacyAgent(t, s, id.NewAgentID().String(), "acme")

	err := s.rescopeLegacyRows(context.Background(), cortex.MigrateOptions{})
	if !errors.Is(err, cortex.ErrNoRescoper) {
		t.Fatalf("err = %v, want ErrNoRescoper", err)
	}
}

func TestRescope_AbortsBeforeWriting(t *testing.T) {
	s := newTestStore(t)
	agentID := id.NewAgentID().String()
	insertLegacyAgent(t, s, agentID, "acme")

	err := s.rescopeLegacyRows(context.Background(),
		cortex.MigrateOptions{Rescoper: failingRescoper{}})
	if err == nil {
		t.Fatal("a failing rescoper must abort the pass")
	}

	// The row must be untouched. A half-rescoped database is worse than a
	// failed migration, because nothing distinguishes done from not-done.
	if canon := legacyScopeCanon(t, s, "cortex_agents", agentID); canon != "" {
		t.Errorf("scope_canon = %q after a failed rescope, want it untouched", canon)
	}
}

func TestRescope_AppliesAndIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	agentID := id.NewAgentID().String()
	insertLegacyAgent(t, s, agentID, "acme")

	r := fixedRescoper{level: "workspace"}
	if err := s.rescopeLegacyRows(context.Background(), cortex.MigrateOptions{Rescoper: r}); err != nil {
		t.Fatalf("rescope: %v", err)
	}

	parsedID, err := id.ParseAgentID(agentID)
	if err != nil {
		t.Fatalf("parse agent id: %v", err)
	}
	got, err := s.Get(context.Background(), parsedID)
	if err != nil {
		t.Fatalf("get rescoped agent: %v", err)
	}
	if got.Scope.Canonical() != "workspace=acme" {
		t.Errorf("scope = %q, want workspace=acme", got.Scope.Canonical())
	}

	// Re-running touches only rows with an empty scope_canon, so a second
	// pass is a no-op rather than a double-application.
	if err := s.rescopeLegacyRows(context.Background(), cortex.MigrateOptions{}); err != nil {
		t.Fatalf("second pass must be a no-op, got: %v", err)
	}
}

func TestRescope_DetectsNameCollision(t *testing.T) {
	s := newTestStore(t)
	insertLegacyAgent(t, s, id.NewAgentID().String(), "acme")
	insertLegacyAgent(t, s, id.NewAgentID().String(), "globex")

	// Both apps map onto ONE scope, so both agents would land on
	// (scope_canon, name) = ("workspace=shared", "assistant").
	r := stubScopeRescoper{scope: cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "shared"}}}}

	err := s.rescopeLegacyRows(context.Background(), cortex.MigrateOptions{Rescoper: r})
	if err == nil {
		t.Fatal("two agents colliding on (scope, name) must abort the pass")
	}
	if !strings.Contains(err.Error(), "assistant") {
		t.Errorf("the error must name the colliding row, got: %v", err)
	}
}

// TestRescope_StepsAndToolCallsInheritTheirRunsScope covers a shape the
// brief's own legacyRow sketch doesn't: cortex_steps and cortex_tool_calls
// carry no app_id or tenant_id of their own, only a run_id. If they were
// resolved through the Rescoper directly with empty identifiers, every
// unscoped step from every run would collapse onto the SAME arbitrary
// scope, and ListSteps -- which filters by run_id AND the caller's scope
// -- would never find them again under their actual run's scope. They
// must inherit whatever scope their parent run resolves to instead.
func TestRescope_StepsAndToolCallsInheritTheirRunsScope(t *testing.T) {
	s := newTestStore(t)
	agentID := id.NewAgentID().String()

	runID := id.NewAgentRunID().String()
	insertLegacyRun(t, s, runID, agentID, "tenantA")

	stepID := id.NewStepID().String()
	insertLegacyStep(t, s, stepID, runID)

	toolCallID := id.NewToolCallID().String()
	insertLegacyToolCall(t, s, toolCallID, stepID, runID)

	// cortex_runs only carries tenant_id, never app_id, so the rescoper
	// keys off the tenant here (unlike fixedRescoper, which keys off the
	// app for the agent-table tests above).
	r := tenantRescoper{level: "workspace"}
	if err := s.rescopeLegacyRows(context.Background(), cortex.MigrateOptions{Rescoper: r}); err != nil {
		t.Fatalf("rescope: %v", err)
	}

	runCanon := legacyScopeCanon(t, s, "cortex_runs", runID)
	if runCanon != "workspace=tenantA" {
		t.Fatalf("run scope_canon = %q, want workspace=tenantA", runCanon)
	}
	if got := legacyScopeCanon(t, s, "cortex_steps", stepID); got != runCanon {
		t.Errorf("step scope_canon = %q, want it to match its run's %q", got, runCanon)
	}
	if got := legacyScopeCanon(t, s, "cortex_tool_calls", toolCallID); got != runCanon {
		t.Errorf("tool call scope_canon = %q, want it to match its run's %q", got, runCanon)
	}
}

// TestRescope_StepInheritsFromAlreadyScopedRunNeedsNoRescoper covers the
// case where a run was already rescoped (or created post-v1.8.0 under a
// real scope) and only its step is still legacy. The step never needs the
// Rescoper itself -- it just copies its already-scoped parent -- so this
// must succeed even with no rescoper supplied at all.
func TestRescope_StepInheritsFromAlreadyScopedRunNeedsNoRescoper(t *testing.T) {
	s := newTestStore(t)
	agentID := id.NewAgentID().String()

	runID := id.NewAgentRunID().String()
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_already_scoped"}}}
	insertScopedRun(t, s, runID, agentID, scope)

	stepID := id.NewStepID().String()
	insertLegacyStep(t, s, stepID, runID)

	if err := s.rescopeLegacyRows(context.Background(), cortex.MigrateOptions{}); err != nil {
		t.Fatalf("a step whose run is already scoped must not require a rescoper: %v", err)
	}

	if got := legacyScopeCanon(t, s, "cortex_steps", stepID); got != scope.Canonical() {
		t.Errorf("step scope_canon = %q, want it to match its already-scoped run %q", got, scope.Canonical())
	}
}
