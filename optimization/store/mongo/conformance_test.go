package mongo

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	tcmongo "github.com/testcontainers/testcontainers-go/modules/mongodb"
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/mongodriver"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/internal/state"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func testDatabase(t *testing.T, replica bool) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	image := os.Getenv("CORTEX_TEST_MONGO_IMAGE")
	if image == "" {
		image = "mongo:7.0.41@sha256:8102f674c3d5b5c3b5a248397778b8d148a998c6409f390ff62628eb617f0847"
	}
	var container *tcmongo.MongoDBContainer
	var err error
	if replica {
		container, err = tcmongo.Run(ctx, image, tcmongo.WithReplicaSet("rs0"))
	} else {
		container, err = tcmongo.Run(ctx, image)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		if cleanupErr := container.Terminate(cleanup); cleanupErr != nil {
			t.Error(cleanupErr)
		}
	})
	connection, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(connection)
	if err != nil {
		t.Fatal(err)
	}
	query := uri.Query()
	query.Set("directConnection", "true")
	uri.RawQuery = query.Encode()
	return uri.String()
}

func openDatabase(t *testing.T, uri string) *grove.DB {
	t.Helper()
	driver := mongodriver.New()
	if err := driver.Open(context.Background(), uri, mongodriver.WithDatabase("optimization")); err != nil {
		t.Fatal(err)
	}
	db, err := grove.Open(driver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

func TestConformance(t *testing.T) {
	storetest.Run(t, conformanceFactory(t))
}

func conformanceFactory(t *testing.T) storetest.Factory {
	t.Helper()
	uri := testDatabase(t, true)
	admin := openDatabase(t, uri)
	if err := New(admin).Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return func(t *testing.T, now func() time.Time) store.Store {
		t.Helper()
		for _, collection := range []string{"cortex_optimization_records", "cortex_optimization_chunks", "cortex_optimization_scopes"} {
			if _, err := mongodriver.Unwrap(admin).Collection(collection).DeleteMany(context.Background(), bson.D{}); err != nil {
				t.Fatal(err)
			}
		}
		s := New(openDatabase(t, uri))
		if err := state.WithClock(now)(s.Store); err != nil {
			t.Fatal(err)
		}
		return s
	}
}
