package sqlstore_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/xraph/grove"
	"github.com/xraph/grove/driver"
	"github.com/xraph/grove/drivers/sqlitedriver"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/internal/sqlstore"
	"github.com/xraph/cortex/optimization/store/internal/state"
	"github.com/xraph/cortex/optimization/store/storetest"
)

type countingDriver struct {
	driver.Driver
	rows atomic.Int64
}

func (d *countingDriver) Query(ctx context.Context, query string, args ...any) (driver.Rows, error) {
	rows, err := d.Driver.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return &countingRows{Rows: rows, count: &d.rows}, nil
}

type countingRows struct {
	driver.Rows
	count *atomic.Int64
}

func (r *countingRows) Next() bool {
	if !r.Rows.Next() {
		return false
	}
	r.count.Add(1)
	return true
}

func TestPointReadConsumesOneDatabaseRow(t *testing.T) {
	raw := sqlitedriver.New()
	if err := raw.Open(context.Background(), filepath.Join(t.TempDir(), "lookup.db")); err != nil {
		t.Fatal(err)
	}
	db, err := grove.Open(raw)
	if err != nil {
		t.Fatal(err)
	}
	counter := &countingDriver{Driver: raw}
	s := state.NewBackend(sqlstore.New(db, counter, sqlstore.SQLite))
	t.Cleanup(func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if migrateErr := s.Migrate(context.Background()); migrateErr != nil {
		t.Fatal(migrateErr)
	}
	a, b := storetest.Fixture(t, "lookup"), storetest.Fixture(t, "lookup")
	ctx := cortex.WithScope(context.Background(), a.Experiment.Spec.Scope)
	for _, bundle := range []optimization.ExperimentBundle{a, b} {
		if createErr := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); createErr != nil {
			t.Fatal(createErr)
		}
	}
	counter.rows.Store(0)
	got, err := s.GetExperiment(ctx, a.Experiment.ID)
	if err != nil || got.ID != a.Experiment.ID || counter.rows.Load() != 1 {
		t.Fatalf("point read: %v, %v, database rows %d", got.ID, err, counter.rows.Load())
	}
	if _, err := raw.Exec(ctx, `UPDATE cortex_optimization_records SET body = '{}' WHERE kind = 'experiments' AND item_key = ?`, b.Experiment.ID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetExperiment(ctx, a.Experiment.ID); err != nil {
		t.Fatalf("point read loaded unrelated corrupt record: %v", err)
	}
	if _, err := s.GetExperiment(ctx, b.Experiment.ID); !errors.Is(err, optimization.ErrUnavailable) {
		t.Fatalf("target corruption ignored: %v", err)
	}
	called := false
	if err := s.Transact(ctx, func(store.Tx) error { called = true; return nil }); !errors.Is(err, optimization.ErrUnavailable) || called {
		t.Fatalf("full transaction ignored corruption: %v, callback %v", err, called)
	}
}
