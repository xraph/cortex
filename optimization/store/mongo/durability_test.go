package mongo

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xraph/grove/drivers/mongodriver"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/internal/state"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestStandaloneRefusesTransactions(t *testing.T) {
	s := New(openDatabase(t, testDatabase(t, false)))
	if err := s.Migrate(context.Background()); !errors.Is(err, optimization.ErrUnavailable) {
		t.Fatalf("standalone migration: %v", err)
	}
	bundle := storetest.Fixture(t, "standalone")
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	called := false
	if err := s.Transact(ctx, func(store.Tx) error { called = true; return nil }); !errors.Is(err, optimization.ErrUnavailable) || called {
		t.Fatalf("standalone work admitted: %v, callback %v", err, called)
	}
}

func TestSameInstanceIndependentScopes(t *testing.T) {
	s := New(openDatabase(t, testDatabase(t, true)))
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	storetest.IndependentScopes(t, s)
}

func TestLargeRecordRoundTrip(t *testing.T) {
	s := New(openDatabase(t, testDatabase(t, true)))
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	bundle := storetest.Fixture(t, "large-record")
	task := bundle.Experiment.Spec.Tasks[0]
	task.Input = strings.Repeat("x", 17<<20)
	sealed, err := optimization.SealTaskCase(task)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Experiment.Spec.Tasks[0] = sealed
	bundle.Experiment.EvaluationPlan.CaseDigests[sealed.ID] = sealed.Digest
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	if err = s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetExperiment(ctx, bundle.Experiment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Tasks[0].Input != task.Input || got.Spec.Tasks[0].Digest != sealed.Digest {
		t.Fatal("large record lost content")
	}
}

func TestLeaseTakeoverAcrossProcesses(t *testing.T) {
	storetest.LeaseTakeoverAcrossProcesses(t, func(t *testing.T) string { return testDatabase(t, true) }, func(t *testing.T, uri string, now func() time.Time) store.Store {
		t.Helper()
		s := New(openDatabase(t, uri))
		if err := state.WithClock(now)(s.Store); err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestScopeFenceDoesNotReplayCallbacks(t *testing.T) {
	uri := testDatabase(t, true)
	first, second, other := New(openDatabase(t, uri)), New(openDatabase(t, uri)), New(openDatabase(t, uri))
	if err := first.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, b := storetest.Fixture(t, "fence-a"), storetest.Fixture(t, "fence-b")
	deadline, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ctxA := cortex.WithScope(deadline, a.Experiment.Spec.Scope)
	ctxB := cortex.WithScope(deadline, b.Experiment.Spec.Scope)
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- first.Transact(ctxA, func(tx store.Tx) error { close(entered); <-release; return tx.CreateExperiment(a) })
	}()
	select {
	case <-entered:
	case err := <-finished:
		t.Fatalf("initial fence: %v", err)
	}
	defer func() {
		close(release)
		if err := <-finished; err != nil {
			t.Error(err)
		}
	}()
	if err := other.Transact(ctxB, func(tx store.Tx) error { return tx.CreateExperiment(b) }); err != nil {
		t.Fatalf("unrelated scope blocked: %v", err)
	}
	called := false
	err := second.Transact(ctxA, func(store.Tx) error { called = true; return nil })
	if !errors.Is(err, optimization.ErrConflict) || called {
		t.Fatalf("contended fence: %v, callback %v", err, called)
	}
}

func TestDatabaseFailureAndMissingChunkRefusePartialState(t *testing.T) {
	db := openDatabase(t, testDatabase(t, true))
	s := New(db)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	mdb := mongodriver.Unwrap(db)
	ctx := context.Background()
	command := bson.D{{Key: "collMod", Value: recordCollection}, {Key: "validator", Value: bson.D{{Key: "kind", Value: bson.D{{Key: "$ne", Value: "events"}}}}}}
	if err := mdb.Database().RunCommand(ctx, command).Err(); err != nil {
		t.Fatal(err)
	}
	bundle := storetest.Fixture(t, "write-failure")
	ctx = cortex.WithScope(ctx, bundle.Experiment.Spec.Scope)
	if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err == nil {
		t.Fatal("database audit failure ignored")
	}
	for _, name := range []string{recordCollection, chunkCollection, scopeCollection} {
		count, err := mdb.Collection(name).CountDocuments(ctx, bson.D{})
		if err != nil || count != 0 {
			t.Fatalf("partial %s commit: %d, %v", name, count, err)
		}
	}
	if err := mdb.Database().RunCommand(ctx, bson.D{{Key: "collMod", Value: recordCollection}, {Key: "validator", Value: bson.D{}}}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
		t.Fatal(err)
	}
	recordID := (state.RecordKey{Kind: "experiments", Key: bundle.Experiment.ID.String()}).ID()
	if _, err := mdb.Collection(chunkCollection).DeleteOne(ctx, bson.D{{Key: "record_id", Value: recordID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetExperiment(ctx, bundle.Experiment.ID); !errors.Is(err, optimization.ErrUnavailable) {
		t.Fatalf("missing chunk read: %v", err)
	}
	called := false
	if err := s.Transact(ctx, func(store.Tx) error { called = true; return nil }); !errors.Is(err, optimization.ErrUnavailable) || called {
		t.Fatalf("missing chunk mutation: %v, callback %v", err, called)
	}
}
