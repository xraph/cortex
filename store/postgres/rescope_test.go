package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/pgdriver"

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

// TestRescope runs every rescope scenario as a subtest of one container,
// clearing the tables the rescope pass touches between each -- a fresh
// postgres container per scenario would work too, but costs minutes for
// no isolation benefit truncateRescopeTables doesn't already provide.
func TestRescope(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	pgContainer, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("cortex_rescope"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := pgContainer.Terminate(context.Background()); cleanupErr != nil {
			t.Logf("terminate postgres container: %v", cleanupErr)
		}
	})

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres connection string: %v", err)
	}

	drv := pgdriver.New()
	if err = drv.Open(ctx, dsn); err != nil {
		t.Fatalf("open pg driver: %v", err)
	}
	db, err := grove.Open(drv)
	if err != nil {
		t.Fatalf("grove open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	s := New(db)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	fresh := func(t *testing.T) *Store {
		t.Helper()
		if _, err := s.pgdb.Exec(ctx, `TRUNCATE TABLE
			cortex_tool_calls, cortex_steps, cortex_checkpoints,
			cortex_memories, cortex_runs, cortex_agents
			RESTART IDENTITY CASCADE`); err != nil {
			t.Fatalf("truncate before subtest: %v", err)
		}
		return s
	}

	t.Run("NoUnscopedRowsNeedsNoRescoper", func(t *testing.T) {
		st := fresh(t)
		if err := st.rescopeLegacyRows(ctx, cortex.MigrateOptions{}); err != nil {
			t.Fatalf("a clean database must not require a rescoper: %v", err)
		}
	})

	t.Run("UnscopedRowsWithoutRescoperAborts", func(t *testing.T) {
		st := fresh(t)
		insertLegacyAgent(t, st, id.NewAgentID().String(), "acme")

		err := st.rescopeLegacyRows(ctx, cortex.MigrateOptions{})
		if !errors.Is(err, cortex.ErrNoRescoper) {
			t.Fatalf("err = %v, want ErrNoRescoper", err)
		}
	})

	t.Run("AbortsBeforeWriting", func(t *testing.T) {
		st := fresh(t)
		agentID := id.NewAgentID().String()
		insertLegacyAgent(t, st, agentID, "acme")

		err := st.rescopeLegacyRows(ctx, cortex.MigrateOptions{Rescoper: failingRescoper{}})
		if err == nil {
			t.Fatal("a failing rescoper must abort the pass")
		}

		// The row must be untouched. A half-rescoped database is worse
		// than a failed migration, because nothing distinguishes done
		// from not-done.
		if canon := legacyScopeCanon(t, st, "cortex_agents", agentID); canon != "" {
			t.Errorf("scope_canon = %q after a failed rescope, want it untouched", canon)
		}
	})

	t.Run("AppliesAndIsIdempotent", func(t *testing.T) {
		st := fresh(t)
		agentID := id.NewAgentID().String()
		insertLegacyAgent(t, st, agentID, "acme")

		r := fixedRescoper{level: "workspace"}
		if err := st.rescopeLegacyRows(ctx, cortex.MigrateOptions{Rescoper: r}); err != nil {
			t.Fatalf("rescope: %v", err)
		}

		parsedID, err := id.ParseAgentID(agentID)
		if err != nil {
			t.Fatalf("parse agent id: %v", err)
		}
		got, err := st.Get(ctx, parsedID)
		if err != nil {
			t.Fatalf("get rescoped agent: %v", err)
		}
		if got.Scope.Canonical() != "workspace=acme" {
			t.Errorf("scope = %q, want workspace=acme", got.Scope.Canonical())
		}

		// Re-running touches only rows with an empty scope_canon, so a
		// second pass is a no-op rather than a double-application.
		if err := st.rescopeLegacyRows(ctx, cortex.MigrateOptions{}); err != nil {
			t.Fatalf("second pass must be a no-op, got: %v", err)
		}
	})

	t.Run("DetectsNameCollision", func(t *testing.T) {
		st := fresh(t)
		insertLegacyAgent(t, st, id.NewAgentID().String(), "acme")
		insertLegacyAgent(t, st, id.NewAgentID().String(), "globex")

		// Both apps map onto ONE scope, so both agents would land on
		// (scope_canon, name) = ("workspace=shared", "assistant").
		r := stubScopeRescoper{scope: cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "shared"}}}}

		err := st.rescopeLegacyRows(ctx, cortex.MigrateOptions{Rescoper: r})
		if err == nil {
			t.Fatal("two agents colliding on (scope, name) must abort the pass")
		}
		if !strings.Contains(err.Error(), "assistant") {
			t.Errorf("the error must name the colliding row, got: %v", err)
		}
	})

	// A step or tool call carries no app_id or tenant_id of its own, only
	// a run_id. If it were resolved through the Rescoper directly with
	// empty identifiers, every unscoped step from every run would
	// collapse onto the SAME arbitrary scope, and ListSteps -- which
	// filters by run_id AND the caller's scope -- would never find them
	// again under their actual run's scope. They must inherit whatever
	// scope their parent run resolves to instead.
	t.Run("StepsAndToolCallsInheritTheirRunsScope", func(t *testing.T) {
		st := fresh(t)
		// cortex_runs.agent_id is a real FK on postgres, so the parent
		// agent row must exist -- but it's scoped up front so it never
		// shows up as a "root" unscoped row of its own.
		agentID := id.NewAgentID().String()
		insertScopedAgent(t, st, agentID, "acme", cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "acme"}}})

		runID := id.NewAgentRunID().String()
		insertLegacyRun(t, st, runID, agentID, "tenantA")

		stepID := id.NewStepID().String()
		insertLegacyStep(t, st, stepID, runID)

		toolCallID := id.NewToolCallID().String()
		insertLegacyToolCall(t, st, toolCallID, stepID, runID)

		r := tenantRescoper{level: "workspace"}
		if err := st.rescopeLegacyRows(ctx, cortex.MigrateOptions{Rescoper: r}); err != nil {
			t.Fatalf("rescope: %v", err)
		}

		runCanon := legacyScopeCanon(t, st, "cortex_runs", runID)
		if runCanon != "workspace=tenantA" {
			t.Fatalf("run scope_canon = %q, want workspace=tenantA", runCanon)
		}
		if got := legacyScopeCanon(t, st, "cortex_steps", stepID); got != runCanon {
			t.Errorf("step scope_canon = %q, want it to match its run's %q", got, runCanon)
		}
		if got := legacyScopeCanon(t, st, "cortex_tool_calls", toolCallID); got != runCanon {
			t.Errorf("tool call scope_canon = %q, want it to match its run's %q", got, runCanon)
		}
	})

	// A step whose run was already rescoped (or created post-v1.8.0 under
	// a real scope) never needs the Rescoper itself -- it just copies its
	// already-scoped parent -- so this must succeed with no rescoper
	// supplied at all.
	t.Run("StepInheritsFromAlreadyScopedRunNeedsNoRescoper", func(t *testing.T) {
		st := fresh(t)
		agentID := id.NewAgentID().String()
		insertScopedAgent(t, st, agentID, "acme", cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "acme"}}})

		runID := id.NewAgentRunID().String()
		scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_already_scoped"}}}
		insertScopedRun(t, st, runID, agentID, scope)

		stepID := id.NewStepID().String()
		insertLegacyStep(t, st, stepID, runID)

		if err := st.rescopeLegacyRows(ctx, cortex.MigrateOptions{}); err != nil {
			t.Fatalf("a step whose run is already scoped must not require a rescoper: %v", err)
		}

		if got := legacyScopeCanon(t, st, "cortex_steps", stepID); got != scope.Canonical() {
			t.Errorf("step scope_canon = %q, want it to match its already-scoped run %q", got, scope.Canonical())
		}
	})

	// MemoriesAndCheckpointsRescope is coverage cortex_memories and
	// cortex_checkpoints had none of before this round: cortex_memories.id
	// is BIGSERIAL, not TEXT like every other scoped table, so it's the
	// one row shape that actually needs legacyRow.ID to be `any` (see the
	// comment on legacyRow) -- this is the exact database this pass
	// exists to migrate. cortex_checkpoints is covered in the same
	// subtest since it had no coverage either.
	t.Run("MemoriesAndCheckpointsRescope", func(t *testing.T) {
		st := fresh(t)
		agentID := id.NewAgentID().String()
		insertScopedAgent(t, st, agentID, "acme", cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "acme"}}})

		runID := id.NewAgentRunID().String()
		insertLegacyRun(t, st, runID, agentID, "tenantR")

		memID := insertLegacyMemory(t, st, agentID, "tenantM", "conversation", "")

		checkpointID := id.NewCheckpointID().String()
		insertLegacyCheckpoint(t, st, checkpointID, runID, agentID, "tenantC")

		r := tenantRescoper{level: "workspace"}
		if err := st.rescopeLegacyRows(ctx, cortex.MigrateOptions{Rescoper: r}); err != nil {
			t.Fatalf("rescope: %v", err)
		}

		if got := legacyScopeCanon(t, st, "cortex_runs", runID); got != "workspace=tenantR" {
			t.Errorf("run scope_canon = %q, want workspace=tenantR", got)
		}
		if got := legacyScopeCanon(t, st, "cortex_memories", memID); got != "workspace=tenantM" {
			t.Errorf("memory scope_canon = %q, want workspace=tenantM", got)
		}
		if got := legacyScopeCanon(t, st, "cortex_checkpoints", checkpointID); got != "workspace=tenantC" {
			t.Errorf("checkpoint scope_canon = %q, want workspace=tenantC", got)
		}
	})

	// CheckpointWithBlankTenantStillResolvedDirectly is the regression
	// test for Finding 2. cortex_checkpoints has BOTH run_id and
	// tenant_id, so a checkpoint whose tenant_id happens to be blank must
	// still be resolved directly through the Rescoper -- the same as
	// every other checkpoint of the same run -- rather than being
	// misclassified as dependent (a run_id-only row) and silently
	// inheriting its run's scope. The run and checkpoint resolve to
	// deliberately different scopes so a misclassification shows up as
	// the wrong canonical string, not an accident of both landing in the
	// same place.
	t.Run("CheckpointWithBlankTenantStillResolvedDirectly", func(t *testing.T) {
		st := fresh(t)
		agentID := id.NewAgentID().String()
		insertScopedAgent(t, st, agentID, "acme", cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "acme"}}})

		runID := id.NewAgentRunID().String()
		insertLegacyRun(t, st, runID, agentID, "tenantR")

		checkpointID := id.NewCheckpointID().String()
		insertLegacyCheckpoint(t, st, checkpointID, runID, agentID, "")

		r := blankToleratingRescoper{}
		if err := st.rescopeLegacyRows(ctx, cortex.MigrateOptions{Rescoper: r}); err != nil {
			t.Fatalf("rescope: %v", err)
		}

		runCanon := legacyScopeCanon(t, st, "cortex_runs", runID)
		if runCanon != "workspace=tenantR" {
			t.Fatalf("run scope_canon = %q, want workspace=tenantR", runCanon)
		}
		got := legacyScopeCanon(t, st, "cortex_checkpoints", checkpointID)
		if got != "workspace=legacy_default" {
			t.Errorf("checkpoint scope_canon = %q, want workspace=legacy_default (resolved directly); "+
				"got the run's scope %q instead, meaning the checkpoint was wrongly treated as dependent", got, runCanon)
		}
	})

	// DetectsNameCollisionWithExistingRow is Finding 3's name-collision
	// half: detectCollisions used to compare unscoped rows only against
	// each other, so a legacy agent rescoped onto a scope an
	// ALREADY-scoped agent of the same name occupies went undetected
	// until it hit the write.
	t.Run("DetectsNameCollisionWithExistingRow", func(t *testing.T) {
		st := fresh(t)
		// A different app_id than the legacy row below, so the INSERT
		// itself doesn't trip the pre-existing (app_id, name) unique
		// index -- the point is to collide on (scope_canon, name) once
		// rescoped, not on the legacy app-keyed constraint.
		existing := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "acme"}}}
		insertScopedAgent(t, st, id.NewAgentID().String(), "acme_existing_seed", existing)

		insertLegacyAgent(t, st, id.NewAgentID().String(), "acme")

		r := fixedRescoper{level: "workspace"}
		err := st.rescopeLegacyRows(ctx, cortex.MigrateOptions{Rescoper: r})
		if err == nil {
			t.Fatal("a legacy agent colliding with an ALREADY-scoped agent must abort the pass")
		}
		if !strings.Contains(err.Error(), "assistant") {
			t.Errorf("the error must name the colliding row, got: %v", err)
		}
	})

	// DetectsWorkingMemoryCollisionWithExistingRow is Finding 3's
	// working-memory half. cortex_memories has no `name` column, so it
	// never participates in the name-based check -- its uniqueness
	// surface is (agent_id, kind, key, scope_canon) instead, the same
	// composite idx_cortex_memories_working enforces.
	t.Run("DetectsWorkingMemoryCollisionWithExistingRow", func(t *testing.T) {
		st := fresh(t)
		agentID := id.NewAgentID().String()

		existing := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "acme"}}}
		insertScopedWorkingMemory(t, st, agentID, "the-key", existing)

		insertLegacyMemory(t, st, agentID, "acme", "working", "the-key")

		r := tenantRescoper{level: "workspace"}
		err := st.rescopeLegacyRows(ctx, cortex.MigrateOptions{Rescoper: r})
		if err == nil {
			t.Fatal("a legacy working-memory row colliding with an ALREADY-scoped one must abort the pass")
		}
		if !strings.Contains(err.Error(), "the-key") {
			t.Errorf("the error must name the colliding key, got: %v", err)
		}
	})

	// DiscoverySkipsTableWithoutScopeCanon is the entire justification
	// for deriving the table list at runtime instead of hardcoding it: a
	// table that doesn't (or no longer) has a scope_canon column must be
	// skipped, not crash the whole pass trying to SELECT a column that
	// isn't there. The column is restored afterward since this
	// subtest shares its database with every other one in this
	// container.
	t.Run("DiscoverySkipsTableWithoutScopeCanon", func(t *testing.T) {
		st := fresh(t)
		if _, err := st.pgdb.Exec(ctx, `ALTER TABLE cortex_checkpoints DROP COLUMN scope_canon`); err != nil {
			t.Fatalf("drop scope_canon from cortex_checkpoints: %v", err)
		}
		t.Cleanup(func() {
			if _, err := st.pgdb.Exec(context.Background(),
				`ALTER TABLE cortex_checkpoints ADD COLUMN scope_canon TEXT NOT NULL DEFAULT ''`); err != nil {
				t.Fatalf("restore scope_canon on cortex_checkpoints: %v", err)
			}
		})

		agentID := id.NewAgentID().String()
		insertLegacyAgent(t, st, agentID, "acme")

		r := fixedRescoper{level: "workspace"}
		if err := st.rescopeLegacyRows(ctx, cortex.MigrateOptions{Rescoper: r}); err != nil {
			t.Fatalf("a table without scope_canon must be skipped, not fail the whole pass: %v", err)
		}

		if got := legacyScopeCanon(t, st, "cortex_agents", agentID); got != "workspace=acme" {
			t.Errorf("agent scope_canon = %q, want workspace=acme", got)
		}
	})

	// RollsBackOnMidTransactionFailure proves the transactional property
	// directly: AbortsBeforeWriting only proves nothing is written when
	// the pass aborts during the resolve phase, BEFORE BeginTx is ever
	// reached. This calls applyScopes directly with a second row aimed
	// at a table that doesn't exist, so the first row's UPDATE genuinely
	// succeeds inside the transaction before the second one fails it.
	t.Run("RollsBackOnMidTransactionFailure", func(t *testing.T) {
		st := fresh(t)
		agentID := id.NewAgentID().String()
		insertLegacyAgent(t, st, agentID, "acme")

		rs := toRawScope(cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "acme"}}})
		rows := []legacyRow{
			{Table: "cortex_agents", ID: agentID, Name: "assistant"},
			{Table: "cortex_table_that_does_not_exist", ID: "bogus"},
		}
		resolved := map[string]rawScope{
			rows[0].key(): rs,
			rows[1].key(): rs,
		}

		if err := st.applyScopes(ctx, rows, resolved); err == nil {
			t.Fatal("a write against a nonexistent table must fail")
		}

		if got := legacyScopeCanon(t, st, "cortex_agents", agentID); got != "" {
			t.Errorf("scope_canon = %q after a rolled-back transaction, want it untouched "+
				"(the first row's UPDATE succeeded inside the transaction before the second failed it)", got)
		}
	})
}

// insertLegacyAgent writes a row with EMPTY scope columns, mimicking a row
// created before v1.8.0. It bypasses the store's Create because Create
// stamps the context scope unconditionally, which would defeat the whole
// point of exercising the pre-scope state.
func insertLegacyAgent(t *testing.T, s *Store, agentID, appID string) {
	t.Helper()
	const q = `INSERT INTO cortex_agents
	    (id, name, app_id, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon, enabled)
	    VALUES ($1, 'assistant', $2, '', '', '', '{}', '', true)`
	if _, err := s.pgdb.Exec(context.Background(), q, agentID, appID); err != nil {
		t.Fatalf("insert legacy agent: %v", err)
	}
}

// insertScopedAgent writes a cortex_agents row that is already scoped, so
// tests exercising a different row's inheritance path can satisfy
// cortex_runs.agent_id's foreign key without adding an unrelated
// unscoped row to the scan. Every caller only ever needs the one agent
// name, so it's fixed rather than threaded through.
func insertScopedAgent(t *testing.T, s *Store, agentID, appID string, scope cortex.Scope) {
	t.Helper()
	l0, l1, l2, extra := scopeColumns(scope)
	extraJSON, err := json.Marshal(extra)
	if err != nil {
		t.Fatalf("marshal scope_extra: %v", err)
	}
	const q = `INSERT INTO cortex_agents
	    (id, name, app_id, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon, enabled)
	    VALUES ($1, 'assistant', $2, $3, $4, $5, $6::jsonb, $7, true)`
	if _, err := s.pgdb.Exec(context.Background(), q, agentID, appID, l0, l1, l2, extraJSON, scope.Canonical()); err != nil {
		t.Fatalf("insert scoped agent: %v", err)
	}
}

// insertLegacyRun writes an unscoped cortex_runs row directly, the same
// way insertLegacyAgent does for agents.
func insertLegacyRun(t *testing.T, s *Store, runID, agentID, tenantID string) {
	t.Helper()
	const q = `INSERT INTO cortex_runs
	    (id, agent_id, tenant_id, state, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon)
	    VALUES ($1, $2, $3, 'created', '', '', '', '{}', '')`
	if _, err := s.pgdb.Exec(context.Background(), q, runID, agentID, tenantID); err != nil {
		t.Fatalf("insert legacy run: %v", err)
	}
}

// insertScopedRun writes a cortex_runs row that is already scoped, as if
// an earlier rescope pass (or a post-v1.8.0 write) had already handled it.
func insertScopedRun(t *testing.T, s *Store, runID, agentID string, scope cortex.Scope) {
	t.Helper()
	l0, l1, l2, extra := scopeColumns(scope)
	extraJSON, err := json.Marshal(extra)
	if err != nil {
		t.Fatalf("marshal scope_extra: %v", err)
	}
	const q = `INSERT INTO cortex_runs
	    (id, agent_id, tenant_id, state, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon)
	    VALUES ($1, $2, '', 'created', $3, $4, $5, $6::jsonb, $7)`
	if _, err := s.pgdb.Exec(context.Background(), q, runID, agentID, l0, l1, l2, extraJSON, scope.Canonical()); err != nil {
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
	    VALUES ($1, $2, 0, '', '', '', '{}', '')`
	if _, err := s.pgdb.Exec(context.Background(), q, stepID, runID); err != nil {
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
	    VALUES ($1, $2, $3, 'search', '', '', '', '{}', '')`
	if _, err := s.pgdb.Exec(context.Background(), q, toolCallID, stepID, runID); err != nil {
		t.Fatalf("insert legacy tool call: %v", err)
	}
}

// legacyScopeCanon reads scope_canon straight from the table, so tests can
// verify the write path without depending on a scope-filtered read. id is
// `any`, not `string`, because cortex_memories.id is BIGSERIAL while
// every other scoped table's id is TEXT.
func legacyScopeCanon(t *testing.T, s *Store, table string, id any) string {
	t.Helper()
	var canon string
	row := s.pgdb.QueryRow(context.Background(), `SELECT scope_canon FROM `+table+` WHERE id = $1`, id)
	if err := row.Scan(&canon); err != nil {
		t.Fatalf("read back scope_canon from %s: %v", table, err)
	}
	return canon
}

// insertLegacyMemory writes an unscoped cortex_memories row and returns
// its auto-generated BIGSERIAL id -- the one scoped-table id that isn't
// TEXT, which is why legacyRow.ID has to be `any`, not `string` (see the
// comment on legacyRow).
func insertLegacyMemory(t *testing.T, s *Store, agentID, tenantID, kind, key string) int64 {
	t.Helper()
	const q = `INSERT INTO cortex_memories
	    (agent_id, tenant_id, kind, "key", content, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon)
	    VALUES ($1, $2, $3, $4, 'hello', '', '', '', '{}'::jsonb, '')
	    RETURNING id`
	var memID int64
	if err := s.pgdb.QueryRow(context.Background(), q, agentID, tenantID, kind, key).Scan(&memID); err != nil {
		t.Fatalf("insert legacy memory: %v", err)
	}
	return memID
}

// insertScopedWorkingMemory writes an already-scoped cortex_memories row
// with kind='working', so tests can construct a working-memory collision
// against a row this pass never touches.
func insertScopedWorkingMemory(t *testing.T, s *Store, agentID, key string, scope cortex.Scope) {
	t.Helper()
	l0, l1, l2, extra := scopeColumns(scope)
	extraJSON, err := json.Marshal(extra)
	if err != nil {
		t.Fatalf("marshal scope_extra: %v", err)
	}
	const q = `INSERT INTO cortex_memories
	    (agent_id, tenant_id, kind, "key", content, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon)
	    VALUES ($1, '', 'working', $2, 'hello', $3, $4, $5, $6::jsonb, $7)`
	if _, err := s.pgdb.Exec(context.Background(), q, agentID, key, l0, l1, l2, extraJSON, scope.Canonical()); err != nil {
		t.Fatalf("insert scoped working memory: %v", err)
	}
}

// insertLegacyCheckpoint writes an unscoped cortex_checkpoints row. Like
// cortex_runs and cortex_memories it carries its own tenant_id, so --
// unlike cortex_steps/cortex_tool_calls -- it must always be resolved
// directly through the Rescoper, never inherited from its run, even
// though it also has a run_id. run_id and agent_id are real foreign keys
// on postgres, so callers must have already inserted both.
func insertLegacyCheckpoint(t *testing.T, s *Store, checkpointID, runID, agentID, tenantID string) {
	t.Helper()
	const q = `INSERT INTO cortex_checkpoints
	    (id, run_id, agent_id, tenant_id, state, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon)
	    VALUES ($1, $2, $3, $4, 'pending', '', '', '', '{}', '')`
	if _, err := s.pgdb.Exec(context.Background(), q, checkpointID, runID, agentID, tenantID); err != nil {
		t.Fatalf("insert legacy checkpoint: %v", err)
	}
}
