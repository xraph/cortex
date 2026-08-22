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

// mongoConformanceImage pins the mongod build used for TestConformance. The
// task's literal "mongodb/mongodb-community-server:8.3.2" tag doesn't
// exist on Docker Hub for this image — mongodb-community-server only
// publishes OS-qualified tags (verified: `docker manifest inspect
// mongodb/mongodb-community-server:8.3.2` 404s, while the -ubi9-slim
// variant resolves). This is the same 8.3.2 build the prior wave used,
// just with the OS suffix Docker Hub actually requires.
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
	mongoContainer, err := tcmongodb.Run(ctx, mongoConformanceImage)
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
func clearConformanceCollections(ctx context.Context, s *Store) error {
	for _, col := range []string{colAgents, colRuns, colSteps, colToolCalls, colMemories, colCheckpoints} {
		if _, err := s.mdb.Collection(col).DeleteMany(ctx, bson.M{}); err != nil {
			return fmt.Errorf("clear collection %s: %w", col, err)
		}
	}
	return nil
}
