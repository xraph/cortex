package sqlite

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestPublicationRecovery(t *testing.T) {
	testRecovery(t, storetest.PublicationRecovery)
}

func TestActivationRecovery(t *testing.T) {
	testRecovery(t, storetest.ActivationRecovery)
}

func TestParallelRecovery(t *testing.T) {
	testRecovery(t, storetest.ParallelRecovery)
}

func testRecovery(t *testing.T, verify func(*testing.T, func(*testing.T, func() time.Time) store.Store)) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "publication.db")
	verify(t, func(t *testing.T, now func() time.Time) store.Store { return openTestStore(t, path, now) })
}

func TestImprovementRecovery(t *testing.T) { testRecovery(t, storetest.ImprovementRecovery) }

func TestImprovementValidationRecovery(t *testing.T) {
	testRecovery(t, storetest.ImprovementValidationRecovery)
}
