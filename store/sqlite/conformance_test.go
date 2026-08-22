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
	storetest.Conformance(t, func(t *testing.T) store.Store {
		return newTestStore(t)
	})
}
