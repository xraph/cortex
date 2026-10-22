package postgres

import (
	"context"
	"testing"
	"time"

	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/pgdriver"

	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/internal/state"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func testDatabase(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := tcpostgres.Run(ctx, "postgres:16-alpine", tcpostgres.WithDatabase("optimization"), tcpostgres.BasicWaitStrategies())
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
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

func openDatabase(t *testing.T, dsn string) *grove.DB {
	t.Helper()
	driver := pgdriver.New()
	if err := driver.Open(context.Background(), dsn); err != nil {
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
	dsn := testDatabase(t)
	admin := openDatabase(t, dsn)
	if err := New(admin).Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	storetest.Run(t, func(t *testing.T, now func() time.Time) store.Store {
		t.Helper()
		if _, err := pgdriver.Unwrap(admin).Exec(context.Background(), `TRUNCATE cortex_optimization_records, cortex_optimization_scopes`); err != nil {
			t.Fatal(err)
		}
		s := New(openDatabase(t, dsn))
		if err := state.WithClock(now)(s.Store); err != nil {
			t.Fatal(err)
		}
		return s
	})
}
