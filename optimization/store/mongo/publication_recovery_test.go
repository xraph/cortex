package mongo

import (
	"context"
	"testing"
	"time"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/mongodriver"

	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/internal/state"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestPublicationRecovery(t *testing.T) {
	testRecovery(t, storetest.PublicationRecovery)
}

func TestActivationRecovery(t *testing.T) {
	testRecovery(t, storetest.ActivationRecovery)
}

func testRecovery(t *testing.T, verify func(*testing.T, func(*testing.T, func() time.Time) store.Store)) {
	t.Helper()
	database := testDatabase(t, true)
	verify(t, func(t *testing.T, now func() time.Time) store.Store {
		t.Helper()
		driver := mongodriver.New()
		if err := driver.Open(context.Background(), database, mongodriver.WithDatabase("optimization")); err != nil {
			t.Fatal(err)
		}
		db, err := grove.Open(driver)
		if err != nil {
			t.Fatal(err)
		}
		s := New(db)
		t.Cleanup(func() {
			if closeErr := s.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
		if err := state.WithClock(now)(s.Store); err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		return s
	})
}
