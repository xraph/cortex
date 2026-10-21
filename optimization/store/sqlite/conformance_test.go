package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/internal/state"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func openTestStore(t *testing.T, path string, now func() time.Time) *Store {
	t.Helper()
	driver := sqlitedriver.New()
	if err := driver.Open(context.Background(), path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"); err != nil {
		t.Fatal(err)
	}
	db, err := grove.Open(driver)
	if err != nil {
		t.Fatal(err)
	}
	s := New(db)
	if err = state.WithClock(now)(s.Store); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := s.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return s
}

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T, now func() time.Time) store.Store {
		return openTestStore(t, filepath.Join(t.TempDir(), "optimizer.db"), now)
	})
}

func TestSeparateConnectionsSeeCommittedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "optimizer.db")
	first := openTestStore(t, path, time.Now)
	second := openTestStore(t, path, time.Now)
	bundle := storetest.Fixture(t, "persistent")
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	if err := first.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
		t.Fatal(err)
	}
	got, err := second.GetExperiment(ctx, bundle.Experiment.ID)
	if err != nil || got.ID != bundle.Experiment.ID {
		t.Fatalf("second connection read: %v, %v", got.ID, err)
	}
	events, err := second.ListEvents(ctx, got.ID, 0, 10)
	if err != nil || len(events.Items) != 1 || events.Items[0].Type != "draft" {
		t.Fatalf("atomic audit missing: %v, %v", events, err)
	}
	if err = second.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); !errors.Is(err, optimization.ErrConflict) {
		t.Fatalf("duplicate committed identity: %v", err)
	}
	if err = second.Migrate(context.Background()); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
}
