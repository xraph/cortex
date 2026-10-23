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
	t.Run("VerificationJobRollback", func(t *testing.T) {
		s := newTestStore(t)
		storetest.VerificationJobRollback(t, s, assuranceDriver{store: s})
	})
	t.Run("VerificationJobLeases", func(t *testing.T) {
		s := newTestStore(t)
		storetest.VerificationJobLeases(t, s, assuranceDriver{store: s})
	})
	t.Run("VerificationJobTermination", func(t *testing.T) {
		s := newTestStore(t)
		storetest.VerificationJobTermination(t, s, assuranceDriver{store: s})
	})
	t.Run("VerificationJobConcurrentClaims", func(t *testing.T) { s := newTestStore(t); storetest.VerificationJobConcurrentClaims(t, s) })
	t.Run("VerifiedResultRollback", func(t *testing.T) {
		s := newTestStore(t)
		storetest.VerifiedResultRollback(t, s, assuranceDriver{store: s})
	})
	storetest.Conformance(t, func(t *testing.T) store.Store {
		return newTestStore(t)
	})
}
