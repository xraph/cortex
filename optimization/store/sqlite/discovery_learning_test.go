package sqlite

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestDiscoveryLearning(t *testing.T) {
	storetest.DiscoveryLearning(t, func(t *testing.T, now func() time.Time) store.Store {
		return openTestStore(t, filepath.Join(t.TempDir(), "discovery.db"), now)
	})
}
func TestDiscoveryLearningRecovery(t *testing.T) {
	testRecovery(t, storetest.DiscoveryLearningRecovery)
}

func TestDiscoveryInheritance(t *testing.T) {
	storetest.DiscoveryInheritance(t, func(t *testing.T, now func() time.Time) store.Store {
		return openTestStore(t, filepath.Join(t.TempDir(), "inheritance.db"), now)
	})
}
func TestDiscoveryInheritanceRecovery(t *testing.T) {
	testRecovery(t, storetest.DiscoveryInheritanceRecovery)
}

func TestDiscoveryReflection(t *testing.T) {
	storetest.DiscoveryReflection(t, func(t *testing.T, now func() time.Time) store.Store {
		return openTestStore(t, filepath.Join(t.TempDir(), "reflection.db"), now)
	})
}
func TestDiscoveryReflectionRecovery(t *testing.T) {
	testRecovery(t, storetest.DiscoveryReflectionRecovery)
}

func TestImprovementWorker(t *testing.T) {
	storetest.ImprovementWorker(t, func(t *testing.T, now func() time.Time) store.Store {
		return openTestStore(t, filepath.Join(t.TempDir(), "parent.db"), now)
	})
}
func TestImprovementWorkerLoop(t *testing.T) {
	storetest.ImprovementWorkerLoop(t, func(t *testing.T, now func() time.Time) store.Store {
		return openTestStore(t, filepath.Join(t.TempDir(), "parent-loop.db"), now)
	})
}
func TestImprovementWorkerRecovery(t *testing.T) {
	testRecovery(t, storetest.ImprovementWorkerRecovery)
}
