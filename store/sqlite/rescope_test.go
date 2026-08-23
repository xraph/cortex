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
// verify the write path without depending on a scope-filtered read. id is
// `any`, not `string`, because cortex_memories.id is INTEGER AUTOINCREMENT
// while every other scoped table's id is TEXT.
func legacyScopeCanon(t *testing.T, s *Store, table string, id any) string {
	t.Helper()
	var canon string
	err := s.sdb.QueryRow(context.Background(),
		`SELECT scope_canon FROM `+table+` WHERE id = ?`, id).Scan(&canon)
	if err != nil {
		t.Fatalf("read back scope_canon from %s: %v", table, err)
	}
	return canon
}

// insertScopedAgentRow writes a cortex_agents row that is already scoped,
// so tests can construct a collision against a row this pass never
// touches.
func insertScopedAgentRow(t *testing.T, s *Store, agentID, name string, scope cortex.Scope) {
	t.Helper()
	l0, l1, l2, extra := scopeColumns(scope)
	const q = `INSERT INTO cortex_agents
	    (id, name, app_id, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon, enabled)
	    VALUES (?, ?, '', ?, ?, ?, ?, ?, 1)`
	if _, err := s.sdb.Exec(context.Background(), q, agentID, name, l0, l1, l2, extra, scope.Canonical()); err != nil {
		t.Fatalf("insert scoped agent: %v", err)
	}
}

// insertLegacyMemory writes an unscoped cortex_memories row and returns
// its auto-generated integer id. cortex_memories.id is INTEGER
// AUTOINCREMENT on sqlite (and BIGSERIAL on postgres), unlike every
// other scoped table's TEXT id -- this is why legacyRow.ID has to be
// `any`, not `string` (see the comment on legacyRow).
func insertLegacyMemory(t *testing.T, s *Store, agentID, tenantID, kind, key string) int64 {
	t.Helper()
	const q = `INSERT INTO cortex_memories
	    (agent_id, tenant_id, kind, "key", content, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon)
	    VALUES (?, ?, ?, ?, 'hello', '', '', '', '{}', '')`
	res, err := s.sdb.Exec(context.Background(), q, agentID, tenantID, kind, key)
	if err != nil {
		t.Fatalf("insert legacy memory: %v", err)
	}
	memID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return memID
}

// insertScopedWorkingMemory writes an already-scoped cortex_memories row
// with kind='working', so tests can construct a working-memory collision
// against a row this pass never touches.
func insertScopedWorkingMemory(t *testing.T, s *Store, agentID, key string, scope cortex.Scope) {
	t.Helper()
	l0, l1, l2, extra := scopeColumns(scope)
	const q = `INSERT INTO cortex_memories
	    (agent_id, tenant_id, kind, "key", content, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon)
	    VALUES (?, '', 'working', ?, 'hello', ?, ?, ?, ?, ?)`
	if _, err := s.sdb.Exec(context.Background(), q, agentID, key, l0, l1, l2, extra, scope.Canonical()); err != nil {
		t.Fatalf("insert scoped working memory: %v", err)
	}
}

// insertLegacyCheckpoint writes an unscoped cortex_checkpoints row. Like
// cortex_runs and cortex_memories it carries its own tenant_id, so --
// unlike cortex_steps/cortex_tool_calls -- it must always be resolved
// directly through the Rescoper, never inherited from its run, even
// though it also has a run_id.
func insertLegacyCheckpoint(t *testing.T, s *Store, checkpointID, runID, agentID, tenantID string) {
	t.Helper()
	const q = `INSERT INTO cortex_checkpoints
	    (id, run_id, agent_id, tenant_id, state, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon)
	    VALUES (?, ?, ?, ?, 'pending', '', '', '', '{}', '')`
	if _, err := s.sdb.Exec(context.Background(), q, checkpointID, runID, agentID, tenantID); err != nil {
		t.Fatalf("insert legacy checkpoint: %v", err)
	}
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

// TestRescope_MemoriesAndCheckpointsRescope is coverage cortex_memories
// and cortex_checkpoints had none of before this round: cortex_memories.id
// is INTEGER AUTOINCREMENT, not TEXT like every other scoped table, so
// it's the one row shape that actually needs legacyRow.ID to be `any`
// (see the comment on legacyRow) -- exactly the database this pass
// exists to migrate. cortex_checkpoints is covered in the same test
// since neither table had any coverage at all before this round.
func TestRescope_MemoriesAndCheckpointsRescope(t *testing.T) {
	s := newTestStore(t)
	agentID := id.NewAgentID().String()

	runID := id.NewAgentRunID().String()
	insertLegacyRun(t, s, runID, agentID, "tenantR")

	memID := insertLegacyMemory(t, s, agentID, "tenantM", "conversation", "")

	checkpointID := id.NewCheckpointID().String()
	insertLegacyCheckpoint(t, s, checkpointID, runID, agentID, "tenantC")

	r := tenantRescoper{level: "workspace"}
	if err := s.rescopeLegacyRows(context.Background(), cortex.MigrateOptions{Rescoper: r}); err != nil {
		t.Fatalf("rescope: %v", err)
	}

	if got := legacyScopeCanon(t, s, "cortex_runs", runID); got != "workspace=tenantR" {
		t.Errorf("run scope_canon = %q, want workspace=tenantR", got)
	}
	if got := legacyScopeCanon(t, s, "cortex_memories", memID); got != "workspace=tenantM" {
		t.Errorf("memory scope_canon = %q, want workspace=tenantM", got)
	}
	if got := legacyScopeCanon(t, s, "cortex_checkpoints", checkpointID); got != "workspace=tenantC" {
		t.Errorf("checkpoint scope_canon = %q, want workspace=tenantC", got)
	}
}

// TestRescope_CheckpointWithBlankTenantStillResolvedDirectly is the
// regression test for Finding 2. cortex_checkpoints has BOTH run_id and
// tenant_id, so a checkpoint whose tenant_id happens to be blank must
// still be resolved directly through the Rescoper -- exactly like every
// other checkpoint of the same run -- rather than being misclassified as
// dependent (a run_id-only row) and silently inheriting its run's scope
// instead. The run and the checkpoint are deliberately given rescopers
// that resolve to DIFFERENT scopes, so a misclassification is visible as
// the wrong canonical string rather than an accident of both landing in
// the same place.
func TestRescope_CheckpointWithBlankTenantStillResolvedDirectly(t *testing.T) {
	s := newTestStore(t)
	agentID := id.NewAgentID().String()

	runID := id.NewAgentRunID().String()
	insertLegacyRun(t, s, runID, agentID, "tenantR")

	checkpointID := id.NewCheckpointID().String()
	insertLegacyCheckpoint(t, s, checkpointID, runID, agentID, "")

	// Maps a blank tenant to a fixed, recognizable scope distinct from
	// whatever the run resolves to, so a misclassified checkpoint that
	// inherited the run's scope instead is unambiguously wrong.
	r := blankToleratingRescoper{}
	if err := s.rescopeLegacyRows(context.Background(), cortex.MigrateOptions{Rescoper: r}); err != nil {
		t.Fatalf("rescope: %v", err)
	}

	runCanon := legacyScopeCanon(t, s, "cortex_runs", runID)
	if runCanon != "workspace=tenantR" {
		t.Fatalf("run scope_canon = %q, want workspace=tenantR", runCanon)
	}
	got := legacyScopeCanon(t, s, "cortex_checkpoints", checkpointID)
	if got != "workspace=legacy_default" {
		t.Errorf("checkpoint scope_canon = %q, want workspace=legacy_default (resolved directly); "+
			"got the run's scope %q instead, meaning the checkpoint was wrongly treated as dependent", got, runCanon)
	}
}

// blankToleratingRescoper maps an empty tenant to a fixed, recognizable
// value instead of returning an invalid empty-valued level (which
// ValidateRescopedScope would reject), so a test can resolve a row with
// no legacy identifier at all through the Rescoper directly.
type blankToleratingRescoper struct{}

func (blankToleratingRescoper) Rescope(_ context.Context, _, tenantID string) (cortex.Scope, error) {
	v := tenantID
	if v == "" {
		v = "legacy_default"
	}
	return cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: v}}}, nil
}

// TestRescope_DetectsNameCollisionWithExistingRow is the regression test
// for Finding 3's name-collision half: detectCollisions used to compare
// unscoped rows only against each other, so a legacy agent rescoped onto
// a scope an ALREADY-scoped agent of the same name already occupies went
// undetected until the raw UPDATE hit the (currently app_id-based, but
// this doesn't need the DB to actually enforce it to prove the gap)
// unique expectation.
func TestRescope_DetectsNameCollisionWithExistingRow(t *testing.T) {
	s := newTestStore(t)
	existing := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "acme"}}}
	insertScopedAgentRow(t, s, id.NewAgentID().String(), "assistant", existing)

	insertLegacyAgent(t, s, id.NewAgentID().String(), "acme")

	r := fixedRescoper{level: "workspace"}
	err := s.rescopeLegacyRows(context.Background(), cortex.MigrateOptions{Rescoper: r})
	if err == nil {
		t.Fatal("a legacy agent colliding with an ALREADY-scoped agent must abort the pass")
	}
	if !strings.Contains(err.Error(), "assistant") {
		t.Errorf("the error must name the colliding row, got: %v", err)
	}
}

// TestRescope_DetectsWorkingMemoryCollisionWithExistingRow is Finding 3's
// working-memory half. cortex_memories has no `name` column, so it never
// participates in the name-based check at all -- its uniqueness surface
// is (agent_id, kind, key, scope_canon), the same composite
// idx_cortex_memories_working enforces. Without a dedicated pre-check
// against already-scoped rows, a legacy working-memory row could be
// rescoped onto a key an existing row already occupies.
func TestRescope_DetectsWorkingMemoryCollisionWithExistingRow(t *testing.T) {
	s := newTestStore(t)
	agentID := id.NewAgentID().String()

	existing := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "acme"}}}
	insertScopedWorkingMemory(t, s, agentID, "the-key", existing)

	insertLegacyMemory(t, s, agentID, "acme", "working", "the-key")

	r := tenantRescoper{level: "workspace"}
	err := s.rescopeLegacyRows(context.Background(), cortex.MigrateOptions{Rescoper: r})
	if err == nil {
		t.Fatal("a legacy working-memory row colliding with an ALREADY-scoped one must abort the pass")
	}
	if !strings.Contains(err.Error(), "the-key") {
		t.Errorf("the error must name the colliding key, got: %v", err)
	}
}

// TestRescope_DiscoverySkipsTableWithoutScopeCanon is the entire
// justification for deriving the table list at runtime instead of
// hardcoding it: a table that doesn't (or no longer) has a scope_canon
// column must be skipped, not crash the whole pass trying to SELECT a
// column that isn't there. Dropping the column from a live table
// simulates the state discoverScopedTables is actually meant to survive
// -- a partially migrated (here: partially DOWNgraded) database.
func TestRescope_DiscoverySkipsTableWithoutScopeCanon(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.sdb.Exec(context.Background(), `ALTER TABLE cortex_checkpoints DROP COLUMN scope_canon`); err != nil {
		t.Fatalf("drop scope_canon from cortex_checkpoints: %v", err)
	}

	agentID := id.NewAgentID().String()
	insertLegacyAgent(t, s, agentID, "acme")

	r := fixedRescoper{level: "workspace"}
	if err := s.rescopeLegacyRows(context.Background(), cortex.MigrateOptions{Rescoper: r}); err != nil {
		t.Fatalf("a table without scope_canon must be skipped, not fail the whole pass: %v", err)
	}

	if got := legacyScopeCanon(t, s, "cortex_agents", agentID); got != "workspace=acme" {
		t.Errorf("agent scope_canon = %q, want workspace=acme", got)
	}
}

// TestRescope_RollsBackOnMidTransactionFailure proves the transactional
// property directly: TestRescope_AbortsBeforeWriting only proves nothing
// is written when the pass aborts during the resolve phase, BEFORE
// BeginTx is ever reached. This calls applyScopes itself with a second
// row aimed at a table that doesn't exist, so the first row's UPDATE
// genuinely succeeds inside the transaction before the second one fails
// it -- and then checks that the first row's success didn't survive the
// rollback.
func TestRescope_RollsBackOnMidTransactionFailure(t *testing.T) {
	s := newTestStore(t)
	agentID := id.NewAgentID().String()
	insertLegacyAgent(t, s, agentID, "acme")

	rs := toRawScope(cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "acme"}}})
	rows := []legacyRow{
		{Table: "cortex_agents", ID: agentID, Name: "assistant"},
		{Table: "cortex_table_that_does_not_exist", ID: "bogus"},
	}
	resolved := map[string]rawScope{
		rows[0].key(): rs,
		rows[1].key(): rs,
	}

	if err := s.applyScopes(context.Background(), rows, resolved); err == nil {
		t.Fatal("a write against a nonexistent table must fail")
	}

	if got := legacyScopeCanon(t, s, "cortex_agents", agentID); got != "" {
		t.Errorf("scope_canon = %q after a rolled-back transaction, want it untouched "+
			"(the first row's UPDATE succeeded inside the transaction before the second failed it)", got)
	}
}
