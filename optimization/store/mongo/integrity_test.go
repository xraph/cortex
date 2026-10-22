package mongo

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/grove/drivers/mongodriver"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/internal/scoped"
	"github.com/xraph/cortex/optimization/store/internal/state"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestChunkCorruptionRefusesReadsAndCallbacks(t *testing.T) {
	db := openDatabase(t, testDatabase(t, true))
	s := New(db)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	mdb := mongodriver.Unwrap(db)
	for _, test := range []struct {
		name, collection, field string
		value                   any
	}{
		{"chunk-content", chunkCollection, "data", []byte("corrupt")},
		{"chunk-digest", chunkCollection, "digest", "changed"},
		{"chunk-index", chunkCollection, "part", int64(-1)},
		{"chunk-identity", chunkCollection, "record_id", "missing"},
		{"chunk-scope", chunkCollection, "scope_canon", "foreign"},
		{"header-size", recordCollection, "body_size", int64(1 << 60)},
		{"header-count", recordCollection, "chunks", int64(0)},
		{"header-schema", recordCollection, "record_schema", 99},
	} {
		t.Run(test.name, func(t *testing.T) {
			bundle := storetest.Fixture(t, test.name)
			ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
			if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
				t.Fatal(err)
			}
			key, err := optimization.ScopeKey(bundle.Experiment.Spec.Scope)
			if err != nil {
				t.Fatal(err)
			}
			scope, err := scoped.New(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			recordID := (state.RecordKey{Kind: "experiments", Key: bundle.Experiment.ID.String()}).ID()
			filter := append(scopeFilter(scope), bson.E{Key: "record_id", Value: recordID})
			if _, err := mdb.Collection(test.collection).UpdateOne(ctx, filter, bson.D{{Key: "$set", Value: bson.D{{Key: test.field, Value: test.value}}}}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetExperiment(ctx, bundle.Experiment.ID); !errors.Is(err, optimization.ErrUnavailable) {
				t.Fatalf("corrupt read: %v", err)
			}
			called := false
			if err := s.Transact(ctx, func(store.Tx) error { called = true; return nil }); !errors.Is(err, optimization.ErrUnavailable) || called {
				t.Fatalf("corrupt write: %v, callback %v", err, called)
			}
		})
	}
}

func TestFailedReplacementPreservesOldChunksAndNoopHistory(t *testing.T) {
	db := openDatabase(t, testDatabase(t, true))
	s := New(db)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	bundle := storetest.Fixture(t, "replace-failure")
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
		t.Fatal(err)
	}
	mdb := mongodriver.Unwrap(db)
	before, err := mdb.Collection(chunkCollection).CountDocuments(ctx, bson.D{})
	if err != nil {
		t.Fatal(err)
	}
	setValidator := func(collection string, validator bson.D) {
		t.Helper()
		if validationErr := mdb.Database().RunCommand(ctx, bson.D{{Key: "collMod", Value: collection}, {Key: "validator", Value: validator}}).Err(); validationErr != nil {
			t.Fatal(validationErr)
		}
	}
	// New event records may be inserted, but replacing the experiment header
	// must fail after its new body chunks have already been written.
	setValidator(recordCollection, bson.D{{Key: "kind", Value: bson.D{{Key: "$ne", Value: "experiments"}}}})
	start := optimization.Transition{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 1, NextState: optimization.StateQueued, IdempotencyKey: "start", Actor: bundle.Experiment.Spec.Requester}
	if transitionErr := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(start) }); transitionErr == nil {
		t.Fatal("header replacement failure ignored")
	}
	after, err := mdb.Collection(chunkCollection).CountDocuments(ctx, bson.D{})
	if err != nil || after != before {
		t.Fatalf("partial chunks: before %d after %d: %v", before, after, err)
	}
	got, err := s.GetExperiment(ctx, bundle.Experiment.ID)
	if err != nil || got.Revision != 1 || got.State != optimization.StateDraft {
		t.Fatalf("original experiment changed: %+v, %v", got, err)
	}
	setValidator(recordCollection, bson.D{})
	if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(start) }); err != nil {
		t.Fatal(err)
	}
	for _, collection := range []string{recordCollection, chunkCollection} {
		setValidator(collection, bson.D{{Key: "_id", Value: bson.D{{Key: "$exists", Value: false}}}})
	}
	if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(start) }); err != nil {
		t.Fatalf("idempotent retry rewrote content: %v", err)
	}
	if err := s.Transact(ctx, func(store.Tx) error { return nil }); err != nil {
		t.Fatalf("no-op rewrote content: %v", err)
	}
	if _, err := s.GetExperiment(ctx, bundle.Experiment.ID); err != nil {
		t.Fatal(err)
	}
}

func TestPointReadIgnoresUnrelatedChunksAndDetectsExtras(t *testing.T) {
	db := openDatabase(t, testDatabase(t, true))
	s := New(db)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, b := storetest.Fixture(t, "lookup"), storetest.Fixture(t, "lookup")
	ctx := cortex.WithScope(context.Background(), a.Experiment.Spec.Scope)
	for _, bundle := range []optimization.ExperimentBundle{a, b} {
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
			t.Fatal(err)
		}
	}
	mdb := mongodriver.Unwrap(db)
	aID := (state.RecordKey{Kind: "experiments", Key: a.Experiment.ID.String()}).ID()
	bID := (state.RecordKey{Kind: "experiments", Key: b.Experiment.ID.String()}).ID()
	if _, err := mdb.Collection(chunkCollection).UpdateOne(ctx, bson.D{{Key: "record_id", Value: bID}}, bson.D{{Key: "$set", Value: bson.D{{Key: "data", Value: []byte("corrupt")}}}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetExperiment(ctx, a.Experiment.ID)
	if err != nil || got.ID != a.Experiment.ID {
		t.Fatalf("point read loaded unrelated chunks: %v, %v", got.ID, err)
	}
	if _, err := s.GetExperiment(ctx, b.Experiment.ID); !errors.Is(err, optimization.ErrUnavailable) {
		t.Fatalf("target corruption ignored: %v", err)
	}
	called := false
	if err := s.Transact(ctx, func(store.Tx) error { called = true; return nil }); !errors.Is(err, optimization.ErrUnavailable) || called {
		t.Fatalf("full transaction ignored corruption: %v, callback %v", err, called)
	}
	var extra chunkDocument
	if err := mdb.Collection(chunkCollection).FindOne(ctx, bson.D{{Key: "record_id", Value: aID}}).Decode(&extra); err != nil {
		t.Fatal(err)
	}
	extra.ID += "/extra"
	extra.Index++
	if _, err := mdb.Collection(chunkCollection).InsertOne(ctx, extra); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetExperiment(ctx, a.Experiment.ID); !errors.Is(err, optimization.ErrUnavailable) {
		t.Fatalf("point read truncated extra chunks: %v", err)
	}
}
