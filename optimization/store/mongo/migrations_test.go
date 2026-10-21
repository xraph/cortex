package mongo

import (
	"context"
	"errors"
	"net/url"
	"testing"

	"github.com/xraph/grove/drivers/mongodriver"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestTransactionOverridesHostReadPreference(t *testing.T) {
	uri, err := url.Parse(testDatabase(t, true))
	if err != nil {
		t.Fatal(err)
	}
	query := uri.Query()
	query.Set("readPreference", "secondaryPreferred")
	uri.RawQuery = query.Encode()
	s := New(openDatabase(t, uri.String()))
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	bundle := storetest.Fixture(t, "primary-transaction")
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetExperiment(ctx, bundle.Experiment.ID); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentMigrationAndFutureSchema(t *testing.T) {
	uri := testDatabase(t, true)
	const workers = 8
	stores := make([]*Store, workers)
	for i := range stores {
		stores[i] = New(openDatabase(t, uri))
	}
	start := make(chan struct{})
	results := make(chan error, workers)
	for _, s := range stores {
		go func() { <-start; results <- s.Migrate(context.Background()) }()
	}
	close(start)
	for range workers {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	db := mongodriver.Unwrap(openDatabase(t, uri))
	if _, err := db.Collection(schemaCollection).UpdateOne(context.Background(), bson.D{{Key: "_id", Value: "schema"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "version", Value: 99}}}}); err != nil {
		t.Fatal(err)
	}
	if err := stores[0].Migrate(context.Background()); !errors.Is(err, optimization.ErrUnavailable) {
		t.Fatalf("future schema accepted: %v", err)
	}
}
