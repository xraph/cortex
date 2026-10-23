package sqlite

import (
	"testing"

	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/storetest"
)

// TestConformance runs the backend-agnostic scope-isolation contract
// against sqlite. No docker involved: newTestStore opens a fresh
// temp-file-backed database per subtest via the existing harness.
func TestConformance(t *testing.T) {
	t.Run("VerifiedResultRollback", func(t *testing.T) {
		s := newTestStore(t)
		storetest.VerifiedResultRollback(t, s, assuranceDriver{store: s})
	})
	storetest.Conformance(t, func(t *testing.T) store.Store {
		return newTestStore(t)
	})
}
