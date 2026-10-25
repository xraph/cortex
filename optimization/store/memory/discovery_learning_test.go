package memory_test

import (
	"testing"
	"time"

	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestDiscoveryLearning(t *testing.T) {
	testDiscovery(t, storetest.DiscoveryLearning)
}

func TestDiscoveryInheritance(t *testing.T) { testDiscovery(t, storetest.DiscoveryInheritance) }

func testDiscovery(t *testing.T, verify func(*testing.T, storetest.Factory)) {
	t.Helper()
	verify(t, func(t *testing.T, now func() time.Time) store.Store {
		t.Helper()
		s, err := memory.New(memory.WithClock(now))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestDiscoveryReflection(t *testing.T) { testDiscovery(t, storetest.DiscoveryReflection) }

func TestImprovementWorker(t *testing.T) { testDiscovery(t, storetest.ImprovementWorker) }

func TestImprovementWorkerLoop(t *testing.T) { testDiscovery(t, storetest.ImprovementWorkerLoop) }
