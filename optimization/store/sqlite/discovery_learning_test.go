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

func TestControllerDecisions(t *testing.T) {
	storetest.ControllerDecisions(t, func(t *testing.T, now func() time.Time) store.Store {
		return openTestStore(t, filepath.Join(t.TempDir(), "controller-decisions.db"), now)
	})
}
func TestControllerDecisionArtifacts(t *testing.T) {
	storetest.ControllerDecisionArtifacts(t, func(t *testing.T, now func() time.Time) store.Store {
		return openTestStore(t, filepath.Join(t.TempDir(), "controller-artifacts.db"), now)
	})
}
func TestControllerDecisionRecovery(t *testing.T) {
	testRecovery(t, storetest.ControllerDecisionRecovery)
}

func TestControllerDecisionWorker(t *testing.T) {
	storetest.ControllerDecisionWorker(t, func(t *testing.T, now func() time.Time) store.Store {
		return openTestStore(t, filepath.Join(t.TempDir(), "controller-worker.db"), now)
	})
}

func TestControllerAdoption(t *testing.T) {
	storetest.ControllerAdoption(t, func(t *testing.T, now func() time.Time) store.Store {
		return openTestStore(t, filepath.Join(t.TempDir(), "controller-adoption.db"), now)
	})
}
func TestControllerAdoptionRecovery(t *testing.T) {
	testRecovery(t, storetest.ControllerAdoptionRecovery)
}

func TestControllerAdoptionArtifactDenial(t *testing.T) {
	storetest.ControllerAdoptionArtifactDenial(t, func(t *testing.T, now func() time.Time) store.Store {
		return openTestStore(t, filepath.Join(t.TempDir(), "controller-artifact.db"), now)
	})
}
