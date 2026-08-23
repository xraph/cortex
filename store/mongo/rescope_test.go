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
