package memory_test

import (
	"testing"
	"time"

	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestDiscoveryLearning(t *testing.T) {
	storetest.DiscoveryLearning(t, func(t *testing.T, now func() time.Time) store.Store {
		t.Helper()
		s, err := memory.New(memory.WithClock(now))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}
