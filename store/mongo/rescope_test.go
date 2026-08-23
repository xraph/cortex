package mongo

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcmongodb "github.com/testcontainers/testcontainers-go/modules/mongodb"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/mongodriver"

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
// clearing the collections the rescope pass touches between each -- a
// fresh mongo container per scenario would work too, but costs minutes
// for no isolation benefit clearRescopeCollections doesn't already
// provide. The testcontainers mongodb module always brings the container
// up as a (single-node) replica set, which is what makes the
// multi-document transaction in applyScopes possible at all.
func TestRescope(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	// applyScopes runs its writes inside a multi-document transaction,
	// which mongo only supports on a replica set (or sharded cluster) --
	// never on a standalone mongod, which is what tcmongodb.Run gives by
	// default. WithReplicaSet is what the rest of this package's
	// TestConformance omits, since scope reads/writes there never need a
	// transaction.
	mongoContainer, err := tcmongodb.Run(ctx, mongoConformanceImage, tcmongodb.WithReplicaSet("rs0"))
	if err != nil {
		t.Fatalf("start mongodb container: %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := mongoContainer.Terminate(context.Background()); cleanupErr != nil {
			t.Logf("terminate mongodb container: %v", cleanupErr)
		}
	})

	uri, err := mongoContainer.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("mongodb connection string: %v", err)
	}
	// With replicaSet set in the URI, the driver does full topology
	// discovery and learns the member's address from the container's own
	// view of itself -- an internal Docker IP that's unreachable from the
	// test process on Docker Desktop (macOS/Windows), even though the
	// mapped host:port works fine. directConnection skips discovery and
	// talks to the mapped address directly; the server is still a real
	// (single-node) replica set, so transactions still work.
	uri += "&directConnection=true"

	drv := mongodriver.New()
	if err = drv.Open(ctx, uri, mongodriver.WithDatabase("cortex_rescope")); err != nil {
		t.Fatalf("open mongo driver: %v", err)
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
		for _, col := range []string{colAgents, colRuns, colSteps, colToolCalls, colMemories, colCheckpoints} {
			if _, err := s.mdb.Collection(col).DeleteMany(ctx, bson.M{}); err != nil {
				t.Fatalf("clear collection %s before subtest: %v", col, err)
			}
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
		if canon := legacyScopeCanon(t, st, colAgents, agentID); canon != "" {
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

		if got := legacyScopeCanon(t, st, colAgents, agentID); got != "workspace=acme" {
			t.Errorf("scope_canon = %q, want workspace=acme", got)
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
		agentID := id.NewAgentID().String()

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

		runCanon := legacyScopeCanon(t, st, colRuns, runID)
		if runCanon != "workspace=tenantA" {
			t.Fatalf("run scope_canon = %q, want workspace=tenantA", runCanon)
		}
		if got := legacyScopeCanon(t, st, colSteps, stepID); got != runCanon {
			t.Errorf("step scope_canon = %q, want it to match its run's %q", got, runCanon)
		}
		if got := legacyScopeCanon(t, st, colToolCalls, toolCallID); got != runCanon {
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

		runID := id.NewAgentRunID().String()
		scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_already_scoped"}}}
		insertScopedRun(t, st, runID, agentID, scope)

		stepID := id.NewStepID().String()
		insertLegacyStep(t, st, stepID, runID)

		if err := st.rescopeLegacyRows(ctx, cortex.MigrateOptions{}); err != nil {
			t.Fatalf("a step whose run is already scoped must not require a rescoper: %v", err)
		}

		if got := legacyScopeCanon(t, st, colSteps, stepID); got != scope.Canonical() {
			t.Errorf("step scope_canon = %q, want it to match its already-scoped run %q", got, scope.Canonical())
		}
	})

	// MemoriesAndCheckpointsRescope gives cortex_memories and
	// cortex_checkpoints the same coverage every other collection has had
	// since round one -- neither had any before this round. Mongo's _id
	// is always a string TypeID here (Finding 1 is a postgres/sqlite-only
	// hazard), but the collections themselves were still completely
	// unexercised.
	t.Run("MemoriesAndCheckpointsRescope", func(t *testing.T) {
		st := fresh(t)
		agentID := id.NewAgentID().String()

		runID := id.NewAgentRunID().String()
		insertLegacyRun(t, st, runID, agentID, "tenantR")

		memID := insertLegacyMemory(t, st, agentID, "tenantM", "conversation", "")

		checkpointID := id.NewCheckpointID().String()
		insertLegacyCheckpoint(t, st, checkpointID, runID, agentID, "tenantC")

		r := tenantRescoper{level: "workspace"}
		if err := st.rescopeLegacyRows(ctx, cortex.MigrateOptions{Rescoper: r}); err != nil {
			t.Fatalf("rescope: %v", err)
		}

		if got := legacyScopeCanon(t, st, colRuns, runID); got != "workspace=tenantR" {
			t.Errorf("run scope_canon = %q, want workspace=tenantR", got)
		}
		if got := legacyScopeCanon(t, st, colMemories, memID); got != "workspace=tenantM" {
			t.Errorf("memory scope_canon = %q, want workspace=tenantM", got)
		}
		if got := legacyScopeCanon(t, st, colCheckpoints, checkpointID); got != "workspace=tenantC" {
			t.Errorf("checkpoint scope_canon = %q, want workspace=tenantC", got)
		}
	})

	// CheckpointWithBlankTenantStillResolvedDirectly is the regression
	// test for Finding 2. cortex_checkpoints documents can carry both a
	// run_id and a tenant_id, so a checkpoint whose tenant_id happens to
	// be blank must still be resolved directly through the Rescoper --
	// the same as every other checkpoint of the same run -- rather than
	// being misclassified as dependent and silently inheriting its run's
	// scope. dependentCollections classifies by collection identity
	// (colCheckpoints is never in it), not by this row's own blank
	// tenant_id, so this must resolve to the Rescoper's own answer for a
	// blank tenant, not the run's scope.
	t.Run("CheckpointWithBlankTenantStillResolvedDirectly", func(t *testing.T) {
		st := fresh(t)
		agentID := id.NewAgentID().String()

		runID := id.NewAgentRunID().String()
		insertLegacyRun(t, st, runID, agentID, "tenantR")

		checkpointID := id.NewCheckpointID().String()
		insertLegacyCheckpoint(t, st, checkpointID, runID, agentID, "")

		r := blankToleratingRescoper{}
		if err := st.rescopeLegacyRows(ctx, cortex.MigrateOptions{Rescoper: r}); err != nil {
			t.Fatalf("rescope: %v", err)
		}

		runCanon := legacyScopeCanon(t, st, colRuns, runID)
		if runCanon != "workspace=tenantR" {
			t.Fatalf("run scope_canon = %q, want workspace=tenantR", runCanon)
		}
		got := legacyScopeCanon(t, st, colCheckpoints, checkpointID)
		if got != "workspace=legacy_default" {
			t.Errorf("checkpoint scope_canon = %q, want workspace=legacy_default (resolved directly); "+
				"got the run's scope %q instead, meaning the checkpoint was wrongly treated as dependent", got, runCanon)
		}
	})

	// DetectsNameCollisionWithExistingRow is Finding 3's name-collision
	// half: detectCollisions used to compare unscoped rows only against
	// each other, so a legacy agent rescoped onto a scope an
	// ALREADY-scoped agent of the same name occupies went undetected.
	t.Run("DetectsNameCollisionWithExistingRow", func(t *testing.T) {
		st := fresh(t)
		existing := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "acme"}}}
		insertScopedAgent(t, st, id.NewAgentID().String(), "acme_existing_seed", "assistant", existing)

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
	// working-memory half. cortex_memories has no `name` field, so it
	// never participates in the name-based check -- its uniqueness
	// surface is (agent_id, kind, key, scope_canon) instead, the same
	// composite workingMemoryUniqueIndexName enforces.
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

	// DiscoverySkipsCollectionWithoutScopeIndex is the entire
	// justification for deriving the collection list at runtime instead
	// of hardcoding it: a collection that doesn't (or no longer) carry
	// the scope_l0_l1_l2 index must be skipped, not treated as scoped.
	// The index is restored afterward since this subtest shares its
	// database with every other one in this container.
	t.Run("DiscoverySkipsCollectionWithoutScopeIndex", func(t *testing.T) {
		st := fresh(t)
		if err := st.mdb.Collection(colCheckpoints).Indexes().DropOne(ctx, scopeIndexName); err != nil {
			t.Fatalf("drop scope index from cortex_checkpoints: %v", err)
		}
		t.Cleanup(func() {
			if _, err := st.mdb.Collection(colCheckpoints).Indexes().CreateOne(context.Background(), scopeIndex); err != nil {
				t.Fatalf("restore scope index on cortex_checkpoints: %v", err)
			}
		})

		agentID := id.NewAgentID().String()
		insertLegacyAgent(t, st, agentID, "acme")

		r := fixedRescoper{level: "workspace"}
		if err := st.rescopeLegacyRows(ctx, cortex.MigrateOptions{Rescoper: r}); err != nil {
			t.Fatalf("a collection without the scope index must be skipped, not fail the whole pass: %v", err)
		}

		if got := legacyScopeCanon(t, st, colAgents, agentID); got != "workspace=acme" {
			t.Errorf("agent scope_canon = %q, want workspace=acme", got)
		}
	})

	// RollsBackOnMidTransactionFailure proves the transactional property
	// directly: AbortsBeforeWriting only proves nothing is written when
	// the pass aborts during the resolve phase, BEFORE the transaction is
	// ever started. This calls applyScopes directly with a second row
	// aimed at a collection name mongo rejects outright, so the first
	// row's update genuinely succeeds inside the transaction before the
	// second one fails it and the whole transaction aborts.
	t.Run("RollsBackOnMidTransactionFailure", func(t *testing.T) {
		st := fresh(t)
		agentID := id.NewAgentID().String()
		insertLegacyAgent(t, st, agentID, "acme")

		rs := toRawScope(cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "acme"}}})
		rows := []legacyRow{
			{Collection: colAgents, ID: agentID, Name: "assistant"},
			{Collection: "", ID: "bogus"},
		}
		resolved := map[string]rawScope{
			rows[0].key(): rs,
			rows[1].key(): rs,
		}

		if err := st.applyScopes(ctx, rows, resolved); err == nil {
			t.Fatal("a write against an invalid collection name must fail")
		}

		if got := legacyScopeCanon(t, st, colAgents, agentID); got != "" {
			t.Errorf("scope_canon = %q after a rolled-back transaction, want it untouched "+
				"(the first row's update succeeded inside the transaction before the second failed it)", got)
		}
	})
}

// insertLegacyAgent writes a document with EMPTY scope fields, mimicking
// a document created before v1.8.0. It bypasses the store's Create
// because Create stamps the context scope unconditionally, which would
// defeat the whole point of exercising the pre-scope state.
func insertLegacyAgent(t *testing.T, s *Store, agentID, appID string) {
	t.Helper()
	doc := bson.M{
		"_id": agentID, "name": "assistant", "app_id": appID,
		"scope_l0": "", "scope_l1": "", "scope_l2": "", "scope_extra": bson.M{}, "scope_canon": "",
		"enabled": true,
	}
	if _, err := s.mdb.Collection(colAgents).InsertOne(context.Background(), doc); err != nil {
		t.Fatalf("insert legacy agent: %v", err)
	}
}

// insertLegacyRun writes an unscoped cortex_runs document directly, the
// same way insertLegacyAgent does for agents. Mongo carries no foreign
// key, so unlike postgres/sqlite the parent agent doesn't need to exist.
func insertLegacyRun(t *testing.T, s *Store, runID, agentID, tenantID string) {
	t.Helper()
	doc := bson.M{
		"_id": runID, "agent_id": agentID, "tenant_id": tenantID, "state": "created",
		"scope_l0": "", "scope_l1": "", "scope_l2": "", "scope_extra": bson.M{}, "scope_canon": "",
	}
	if _, err := s.mdb.Collection(colRuns).InsertOne(context.Background(), doc); err != nil {
		t.Fatalf("insert legacy run: %v", err)
	}
}

// insertScopedRun writes a cortex_runs document that is already scoped,
// as if an earlier rescope pass (or a post-v1.8.0 write) had already
// handled it.
func insertScopedRun(t *testing.T, s *Store, runID, agentID string, scope cortex.Scope) {
	t.Helper()
	l0, l1, l2, extra := scopeColumns(scope)
	doc := bson.M{
		"_id": runID, "agent_id": agentID, "tenant_id": "", "state": "created",
		"scope_l0": l0, "scope_l1": l1, "scope_l2": l2, "scope_extra": extra, "scope_canon": scope.Canonical(),
	}
	if _, err := s.mdb.Collection(colRuns).InsertOne(context.Background(), doc); err != nil {
		t.Fatalf("insert scoped run: %v", err)
	}
}

// insertLegacyStep writes an unscoped cortex_steps document. Steps carry
// no legacy identifier of their own -- only a run_id -- so rescoping one
// depends entirely on its parent run resolving to a scope.
func insertLegacyStep(t *testing.T, s *Store, stepID, runID string) {
	t.Helper()
	doc := bson.M{
		"_id": stepID, "run_id": runID, "index": 0,
		"scope_l0": "", "scope_l1": "", "scope_l2": "", "scope_extra": bson.M{}, "scope_canon": "",
	}
	if _, err := s.mdb.Collection(colSteps).InsertOne(context.Background(), doc); err != nil {
		t.Fatalf("insert legacy step: %v", err)
	}
}

// insertLegacyToolCall writes an unscoped cortex_tool_calls document,
// which like cortex_steps inherits its scope from run_id rather than
// carrying its own legacy identifier.
func insertLegacyToolCall(t *testing.T, s *Store, toolCallID, stepID, runID string) {
	t.Helper()
	doc := bson.M{
		"_id": toolCallID, "step_id": stepID, "run_id": runID, "tool_name": "search",
		"scope_l0": "", "scope_l1": "", "scope_l2": "", "scope_extra": bson.M{}, "scope_canon": "",
	}
	if _, err := s.mdb.Collection(colToolCalls).InsertOne(context.Background(), doc); err != nil {
		t.Fatalf("insert legacy tool call: %v", err)
	}
}

// insertScopedAgent writes a cortex_agents document that is already
// scoped, so tests exercising a different row's collision can add an
// existing row without it showing up as an unscoped row of its own.
func insertScopedAgent(t *testing.T, s *Store, agentID, appID, name string, scope cortex.Scope) {
	t.Helper()
	l0, l1, l2, extra := scopeColumns(scope)
	doc := bson.M{
		"_id": agentID, "name": name, "app_id": appID,
		"scope_l0": l0, "scope_l1": l1, "scope_l2": l2, "scope_extra": extra, "scope_canon": scope.Canonical(),
		"enabled": true,
	}
	if _, err := s.mdb.Collection(colAgents).InsertOne(context.Background(), doc); err != nil {
		t.Fatalf("insert scoped agent: %v", err)
	}
}

// insertLegacyMemory writes an unscoped cortex_memories document and
// returns its id. Unlike postgres/sqlite, mongo's _id is always a string
// TypeID here (Finding 1 doesn't reproduce on this backend), generated
// the same way the real SaveConversation/SaveWorking paths do.
func insertLegacyMemory(t *testing.T, s *Store, agentID, tenantID, kind, key string) string {
	t.Helper()
	memID := id.NewMemoryID().String()
	doc := bson.M{
		"_id": memID, "agent_id": agentID, "tenant_id": tenantID, "kind": kind, "key": key, "content": "hello",
		"scope_l0": "", "scope_l1": "", "scope_l2": "", "scope_extra": bson.M{}, "scope_canon": "",
	}
	if _, err := s.mdb.Collection(colMemories).InsertOne(context.Background(), doc); err != nil {
		t.Fatalf("insert legacy memory: %v", err)
	}
	return memID
}

// insertScopedWorkingMemory writes an already-scoped cortex_memories
// document with kind='working', so tests can construct a working-memory
// collision against a row this pass never touches.
func insertScopedWorkingMemory(t *testing.T, s *Store, agentID, key string, scope cortex.Scope) {
	t.Helper()
	l0, l1, l2, extra := scopeColumns(scope)
	doc := bson.M{
		"_id": id.NewMemoryID().String(), "agent_id": agentID, "tenant_id": "", "kind": "working", "key": key, "content": "hello",
		"scope_l0": l0, "scope_l1": l1, "scope_l2": l2, "scope_extra": extra, "scope_canon": scope.Canonical(),
	}
	if _, err := s.mdb.Collection(colMemories).InsertOne(context.Background(), doc); err != nil {
		t.Fatalf("insert scoped working memory: %v", err)
	}
}

// insertLegacyCheckpoint writes an unscoped cortex_checkpoints document.
// Like cortex_runs and cortex_memories it can carry its own tenant_id, so
// -- unlike cortex_steps/cortex_tool_calls -- it must always be resolved
// directly through the Rescoper, never inherited from its run, even
// though it also has a run_id.
func insertLegacyCheckpoint(t *testing.T, s *Store, checkpointID, runID, agentID, tenantID string) {
	t.Helper()
	doc := bson.M{
		"_id": checkpointID, "run_id": runID, "agent_id": agentID, "tenant_id": tenantID, "state": "pending",
		"scope_l0": "", "scope_l1": "", "scope_l2": "", "scope_extra": bson.M{}, "scope_canon": "",
	}
	if _, err := s.mdb.Collection(colCheckpoints).InsertOne(context.Background(), doc); err != nil {
		t.Fatalf("insert legacy checkpoint: %v", err)
	}
}

// legacyScopeCanon reads scope_canon straight from the collection, so
// tests can verify the write path without depending on a scope-filtered
// read.
func legacyScopeCanon(t *testing.T, s *Store, col, id string) string {
	t.Helper()
	var doc struct {
		ScopeCanon string `bson:"scope_canon"`
	}
	err := s.mdb.Collection(col).FindOne(context.Background(), bson.M{"_id": id}).Decode(&doc)
	if err != nil {
		t.Fatalf("read back scope_canon from %s: %v", col, err)
	}
	return doc.ScopeCanon
}
