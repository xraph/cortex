package sqlite

import (
	"context"
	"testing"

	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/storetest"
)

// TestConformance runs the backend-agnostic scope-isolation contract
// against sqlite. No docker involved: newTestStore opens a fresh
// temp-file-backed database per subtest via the existing harness.
func TestConformance(t *testing.T) {
	t.Run("OrchestrationContinuationUpgrade", func(t *testing.T) {
		s := newTestStore(t)
		storetest.OrchestrationSQLContinuationUpgrade(t, s, func(query string) error { _, err := s.sdb.Exec(context.Background(), query); return err })
	})
	t.Run("OrchestrationRevisionUpgrade", func(t *testing.T) {
		s := newTestStore(t)
		storetest.OrchestrationSQLRevisionUpgrade(t, s, func(query string) error { _, err := s.sdb.Exec(context.Background(), query); return err })
	})
	t.Run("A2ARoutingUpgrade", func(t *testing.T) {
		s := newTestStore(t)
		storetest.A2ASQLRoutingUpgrade(t, s, func(query string) error { _, err := s.sdb.Exec(context.Background(), query); return err })
	})

	t.Run("TaskRecoveryAdoption", func(t *testing.T) {
		s := newTestStore(t)
		storetest.TaskRecoveryAdoption(t, s, assuranceDriver{store: s})
	})
	t.Run("RunRecoveryTransactions", func(t *testing.T) {
		s := newTestStore(t)
		storetest.RunRecoveryTransactions(t, s, assuranceDriver{store: s})
	})
	t.Run("A2AAskResolutionTransactions", func(t *testing.T) {
		s := newTestStore(t)
		storetest.A2AAskResolutionTransactions(t, s, assuranceDriver{store: s})
	})
	t.Run("A2AHeldReleaseTransactions", func(t *testing.T) {
		s := newTestStore(t)
		storetest.A2AHeldReleaseTransactions(t, s, assuranceDriver{store: s})
	})
	t.Run("SuspensionVersionTransactions", func(t *testing.T) {
		s := newTestStore(t)
		storetest.SuspensionVersionTransactions(t, s, assuranceDriver{store: s})
	})
	t.Run("ResumeAdmissionTransactions", func(t *testing.T) {
		s := newTestStore(t)
		storetest.ResumeAdmissionTransactions(t, s, assuranceDriver{store: s})
	})
	t.Run("A2AReplyTransactions", func(t *testing.T) {
		s := newTestStore(t)
		storetest.A2AReplyTransactions(t, s, assuranceDriver{store: s})
	})
	t.Run("RunCallTransactions", func(t *testing.T) { s := newTestStore(t); storetest.RunCallTransactions(t, assuranceDriver{store: s}) })
	t.Run("TaskRunTransactions", func(t *testing.T) {
		s := newTestStore(t)
		storetest.TaskRunTransactions(t, s, assuranceDriver{store: s})
	})
	t.Run("TaskDispatchTransactions", func(t *testing.T) {
		s := newTestStore(t)
		storetest.TaskDispatchTransactions(t, s, assuranceDriver{store: s})
	})
	t.Run("ReadyClaimTransactions", func(t *testing.T) {
		s := newTestStore(t)
		storetest.ReadyClaimTransactions(t, s, assuranceDriver{store: s})
	})
	t.Run("TaskAcceptanceTransactions", func(t *testing.T) {
		s := newTestStore(t)
		storetest.TaskAcceptanceTransactions(t, s, assuranceDriver{store: s})
	})
	t.Run("BudgetedClaimTransactions", func(t *testing.T) {
		s := newTestStore(t)
		storetest.BudgetedClaimTransactions(t, s, assuranceDriver{store: s})
	})
	t.Run("BudgetTransactions", func(t *testing.T) {
		s := newTestStore(t)
		storetest.BudgetTransactions(t, s, assuranceDriver{store: s})
	})
	t.Run("ClaimTransactions", func(t *testing.T) { s := newTestStore(t); storetest.ClaimTransactions(t, s, assuranceDriver{store: s}) })
	t.Run("ReviewTransactions", func(t *testing.T) {
		s := newTestStore(t)
		storetest.ReviewTransactions(t, s, assuranceDriver{store: s})
	})
	t.Run("SubmissionTransactions", func(t *testing.T) {
		s := newTestStore(t)
		storetest.SubmissionTransactions(t, s, assuranceDriver{store: s})
	})
	t.Run("AssignmentTransactions", func(t *testing.T) {
		s := newTestStore(t)
		storetest.AssignmentTransactions(t, s, assuranceDriver{store: s})
	})
	t.Run("ResultSnapshotBoundaries", func(t *testing.T) {
		s := newTestStore(t)
		storetest.ResultSnapshotBoundaries(t, s, assuranceDriver{store: s})
	})
	t.Run("TerminalEventRollback", func(t *testing.T) {
		s := newTestStore(t)
		storetest.TerminalEventRollback(t, s, assuranceDriver{store: s})
	})
	t.Run("PrivateCandidateRollback", func(t *testing.T) {
		s := newTestStore(t)
		storetest.PrivateCandidateRollback(t, s, assuranceDriver{store: s})
	})
	t.Run("CaptureIntentRollback", func(t *testing.T) {
		s := newTestStore(t)
		storetest.CaptureIntentRollback(t, s, assuranceDriver{store: s})
	})
	t.Run("VerificationCaptureRollback", func(t *testing.T) {
		s := newTestStore(t)
		storetest.VerificationCaptureRollback(t, s, assuranceDriver{store: s})
	})
	t.Run("VerificationCallRecovery", func(t *testing.T) {
		s := newTestStore(t)
		storetest.VerificationCallRecovery(t, s, assuranceDriver{store: s})
	})
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
