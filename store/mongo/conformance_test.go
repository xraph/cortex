package mongo

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcmongodb "github.com/testcontainers/testcontainers-go/modules/mongodb"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/mongodriver"

	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/storetest"
)

// mongoConformanceImage pins the backend test fixture. MongoDB 8.3.2 refuses
// to start on Linux kernels >=6.19, including current hosted CI runners.
// You can test another server with CORTEX_TEST_MONGO_IMAGE; passing this
// fixture does not qualify an untested server version.
const mongoConformanceImage = "mongo:7.0.41@sha256:8102f674c3d5b5c3b5a248397778b8d148a998c6409f390ff62628eb617f0847"

// mongoImage returns the pinned fixture unless you override it explicitly.
func mongoImage() string {
	if img := os.Getenv("CORTEX_TEST_MONGO_IMAGE"); img != "" {
		return img
	}
	return mongoConformanceImage
}

// TestConformance runs the backend-agnostic scope-isolation contract
// (store/storetest) against a real MongoDB instance via testcontainers-go.
//
// SkipIfProviderIsNotHealthy makes this skip cleanly (not fail) when
// Docker isn't available, so a plain `go test ./...` still passes on a
// machine without it, while running for real whenever Docker is present.
func TestConformance(t *testing.T) {
	s := newConformanceStore(t, true)
	ctx := context.Background()

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
	t.Run("HistoricalRunRevision", func(t *testing.T) { testHistoricalRunRevision(t, s) })
	t.Run("TaskRecoveryAdoption", func(t *testing.T) { storetest.TaskRecoveryAdoption(t, s, assuranceDriver{store: s}) })
	t.Run("RunRecoveryTransactions", func(t *testing.T) { storetest.RunRecoveryTransactions(t, s, assuranceDriver{store: s}) })
	t.Run("RunCallTransactions", func(t *testing.T) { storetest.RunCallTransactions(t, assuranceDriver{store: s}) })
	t.Run("TaskRunTransactions", func(t *testing.T) {
		storetest.TaskRunTransactions(t, s, assuranceDriver{store: s})
	})
	t.Run("TaskDispatchTransactions", func(t *testing.T) {
		storetest.TaskDispatchTransactions(t, s, assuranceDriver{store: s})
	})
	t.Run("ReadyClaimTransactions", func(t *testing.T) { storetest.ReadyClaimTransactions(t, s, assuranceDriver{store: s}) })
	t.Run("TaskAcceptanceTransactions", func(t *testing.T) { storetest.TaskAcceptanceTransactions(t, s, assuranceDriver{store: s}) })
	t.Run("BudgetedClaimTransactions", func(t *testing.T) { storetest.BudgetedClaimTransactions(t, s, assuranceDriver{store: s}) })
	t.Run("BudgetTransactions", func(t *testing.T) { storetest.BudgetTransactions(t, s, assuranceDriver{store: s}) })
	t.Run("ClaimTransactions", func(t *testing.T) { storetest.ClaimTransactions(t, s, assuranceDriver{store: s}) })
	t.Run("ReviewTransactions", func(t *testing.T) { storetest.ReviewTransactions(t, s, assuranceDriver{store: s}) })
	t.Run("SubmissionTransactions", func(t *testing.T) { storetest.SubmissionTransactions(t, s, assuranceDriver{store: s}) })
	t.Run("AssignmentTransactions", func(t *testing.T) { storetest.AssignmentTransactions(t, s, assuranceDriver{store: s}) })
	t.Run("VerifiedResultRevocation", func(t *testing.T) { storetest.VerifiedResultRevocation(t, s, assuranceDriver{store: s}) })
	t.Run("PrivateCandidateRollback", func(t *testing.T) { storetest.PrivateCandidateRollback(t, s, assuranceDriver{store: s}) })
	t.Run("CaptureIntentRollback", func(t *testing.T) { storetest.CaptureIntentRollback(t, s, assuranceDriver{store: s}) })
	t.Run("VerificationCaptureRollback", func(t *testing.T) { storetest.VerificationCaptureRollback(t, s, assuranceDriver{store: s}) })
	t.Run("VerificationCallRecovery", func(t *testing.T) { storetest.VerificationCallRecovery(t, s, assuranceDriver{store: s}) })
	t.Run("VerificationJobRollback", func(t *testing.T) { storetest.VerificationJobRollback(t, s, assuranceDriver{store: s}) })
	t.Run("VerificationJobLeases", func(t *testing.T) { storetest.VerificationJobLeases(t, s, assuranceDriver{store: s}) })
	t.Run("VerificationJobTermination", func(t *testing.T) { storetest.VerificationJobTermination(t, s, assuranceDriver{store: s}) })
	t.Run("VerificationJobConcurrentClaims", func(t *testing.T) { storetest.VerificationJobConcurrentClaims(t, s) })
	t.Run("TerminalEventRollback", func(t *testing.T) { storetest.TerminalEventRollback(t, s, assuranceDriver{store: s}) })
	t.Run("ResultSnapshotBoundaries", func(t *testing.T) { storetest.ResultSnapshotBoundaries(t, s, assuranceDriver{store: s}) })
	t.Run("VerifiedResultRollback", func(t *testing.T) { storetest.VerifiedResultRollback(t, s, assuranceDriver{store: s}) })
	t.Run("ScopeInventory", func(t *testing.T) {
		if err := clearConformanceCollections(ctx, s); err != nil {
			t.Fatal(err)
		}
		testScopeInventory(t, s)
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

func newConformanceStore(t *testing.T, replicaSet bool) *Store {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	// Multi-document operations require a replica set. The standalone fixture
	// verifies that result commits fail without transaction support.
	var customizers []testcontainers.ContainerCustomizer
	if replicaSet {
		customizers = append(customizers, tcmongodb.WithReplicaSet("rs0"))
	}
	mongoContainer, err := tcmongodb.Run(ctx, mongoImage(), customizers...)
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
	// Docker Desktop exposes a mapped host port; the replica's advertised
	// container address is unreachable from the host. Keep a direct connection.
	if replicaSet {
		uri += "&directConnection=true"
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
	requireMongoPrimary(t, s)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	return s
}
