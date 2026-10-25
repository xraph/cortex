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
