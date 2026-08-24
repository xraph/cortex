package mongo

import (
	"context"
	"fmt"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcmongodb "github.com/testcontainers/testcontainers-go/modules/mongodb"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/mongodriver"

	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/storetest"
)

// mongoConformanceImage pins the mongod build used for TestConformance.
// The literal "mongodb/mongodb-community-server:8.3.2" tag doesn't exist
// on Docker Hub for this image: mongodb-community-server only publishes
// OS-qualified tags (verified: `docker manifest inspect
// mongodb/mongodb-community-server:8.3.2` 404s, while the -ubi9-slim
// variant resolves). This is the same 8.3.2 build used previously, just
// with the OS suffix Docker Hub actually requires.
const mongoConformanceImage = "mongodb/mongodb-community-server:8.3.2-ubi9-slim"

// TestConformance runs the backend-agnostic scope-isolation contract
// (store/storetest) against a real MongoDB instance via testcontainers-go.
//
// SkipIfProviderIsNotHealthy makes this skip cleanly (not fail) when
// Docker isn't available, so a plain `go test ./...` still passes on a
// machine without it, while running for real whenever Docker is present.
func TestConformance(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	// SaveConversation/ClearConversation run their writes inside a
	// multi-document transaction now (conversation memory maintains its
	// owning session's counters in the same transaction as the message
	// rows), which mongo only supports on a replica set (or sharded
	// cluster) — never on a standalone mongod, which is what
	// tcmongodb.Run gives by default. WithReplicaSet used to be needed
	// only by TestRescope in this package; the conformance suite's own
	// Conversation and SessionMessageCounters subtests now exercise the
	// same requirement.
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
	// (single-node) replica set, so transactions still work. Same fix as
	// TestRescope in this package, now needed here too.
	uri += "&directConnection=true"

	drv := mongodriver.New()
	if err = drv.Open(ctx, uri, mongodriver.WithDatabase("cortex_conformance")); err != nil {
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

	// One container serves the whole run: dropping collection contents
	// between subtests is far cheaper than a fresh container or database
	// per subtest, and mongo carries no schema/FK state that would make
	// reuse unsafe the way it might for a SQL backend.
	storetest.Conformance(t, func(t *testing.T) store.Store {
		t.Helper()
		if err := clearConformanceCollections(ctx, s); err != nil {
			t.Fatalf("clear collections before subtest: %v", err)
		}
		return s
	})
}

// clearConformanceCollections empties every collection store/storetest
// touches so each newStore(t) call hands Conformance a "freshly migrated,
// empty store" per its contract, without paying for a new container or
// database per subtest. Indexes created by Migrate survive a DeleteMany
// and don't need to be recreated.
//
// This list used to stop at colCheckpoints, from when that was every
// entity Conformance exercised. It went stale as skills, traits,
// behaviors, personas, sessions, and orchestration were added without a
// matching entry here — latent, because no subtest before now both wrote
// one of those collections AND ran an unfiltered list against it later in
// the same TestConformance run. storetest's own SessionMessageCounters
// and Conversation subtests do exactly that for cortex_sessions (writing
// stray rows another subtest's ListSessions then picked up), which is
// what surfaced the gap; the fix is to stop it recurring for the rest.
func clearConformanceCollections(ctx context.Context, s *Store) error {
	collections := []string{
		colAgents, colRuns, colSteps, colToolCalls, colMemories, colCheckpoints,
		colSkills, colTraits, colBehaviors, colPersonas, colSessions, colSuspensions, colOverlays,
		colOrchestrationConfigs, colOrchestrationRuns,
	}
	for _, col := range collections {
		if _, err := s.mdb.Collection(col).DeleteMany(ctx, bson.M{}); err != nil {
			return fmt.Errorf("clear collection %s: %w", col, err)
		}
	}
	return nil
}
