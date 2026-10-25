package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/pgdriver"

	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/internal/state"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestPublicationRecovery(t *testing.T) {
	database := testDatabase(t)
	storetest.PublicationRecovery(t, func(t *testing.T, now func() time.Time) store.Store {
		t.Helper()
		driver := pgdriver.New()
		if err := driver.Open(context.Background(), database); err != nil {
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
