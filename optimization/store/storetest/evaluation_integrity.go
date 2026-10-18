package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/run"
)

func evaluationIntegrity(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("FrozenPlanReferenceCannotChangeMetadata", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := Fixture(t, "plan-reuse")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
			t.Fatal(err)
		}
		bundle.Experiment.ID = id.New(optimization.PrefixExperiment)
		bundle.Experiment.EvaluationPlan.Scorers[0].Revision = "2"
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("same frozen plan reference changed scorer: %v", err)
		}
	})
	t.Run("EvaluatedFailureCannotBeRetriedAsSameTrial", func(t *testing.T) {
		s := runningTrial(t, factory)
		record := capturedExecution(t, s.bundle, s.receipt)
		record.State = run.StateFailed
		record.Outcome.StopReason = run.StopExecutionError
		var err error
		record, err = optimization.SealExecution(record)
		if err != nil {
			t.Fatal(err)
		}
		update := s.update(t, optimization.TrialFailed)
		update.Execution = &record
		update.Error = "provider failed"
		commitTrialChange(s.ctx, t, s.store, s.change(t, "capture-failure", update))
		value := evaluationFor(s, record)
		value.Complete = false
		value.Errors = []string{"execution failed"}
		commitTrialChange(s.ctx, t, s.store, evaluationChange(t, s, value))
		stop := optimization.StopReceipt{EnvironmentReceiptID: s.receipt.ID, AttemptID: s.receipt.AttemptID, Fence: s.receipt.Fence, State: optimization.StopQuarantined, Evidence: s.receipt.Snapshot}
		update = s.update(t, optimization.TrialFailed)
		update.Stop = &stop
		update.Error = "failed and quarantined"
		commitTrialChange(s.ctx, t, s.store, s.change(t, "contain-failure", update))
		current, err := s.store.GetTrial(s.ctx, s.trial.ID)
		if err != nil {
			t.Fatal(err)
		}
		retry := optimization.TrialUpdate{TrialID: current.ID, ExpectedRevision: current.Revision, State: optimization.TrialRunning, StartAttemptID: id.New(optimization.PrefixAttempt), RetryApproval: &s.receipt.Snapshot}
		change := s.change(t, "retry-evaluated", retry)
		if err = s.store.Transact(s.ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("retry detached scoring from original result: %v", err)
		}
	})
	t.Run("EvaluationRequiresSeparatelyCommittedExecution", func(t *testing.T) {
		s := runningTrial(t, factory)
		record := capturedExecution(t, s.bundle, s.receipt)
		update := s.update(t, optimization.TrialCompleted)
		update.Execution = &record
		capture := s.change(t, "capture-and-score", update)
		value := evaluationFor(s, record)
		scoring := evaluationChange(t, s, value)
		scoring.ExpectedRevision++
		if err := s.store.Transact(s.ctx, func(tx store.Tx) error {
			if err := tx.CommitTransition(capture); err != nil {
				return err
			}
			return tx.CommitTransition(scoring)
		}); !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("uncommitted execution scored: %v", err)
		}
		trial, err := s.store.GetTrial(s.ctx, s.trial.ID)
		if err != nil || trial.Execution != nil || trial.EvaluationID != "" {
			t.Fatal("failed transaction published capture")
		}
	})
	t.Run("EvaluationIDCannotBackAnotherTrial", func(t *testing.T) {
		s, value := evaluatedTrial(t, factory)
		commitTrialChange(s.ctx, t, s.store, evaluationChange(t, s, value))
		second := queuedTrial(s.bundle, 0)
		second.Repetition = 1
		receipt := openTrial(s.ctx, t, s.store, s.bundle, s.lease, second)
		s.trial = second
		s.receipt = receipt
		record := capturedExecution(t, s.bundle, receipt)
		update := s.update(t, optimization.TrialCompleted)
		update.Execution = &record
		commitTrialChange(s.ctx, t, s.store, s.change(t, "capture-second", update))
		duplicate := evaluationFor(s, record)
		change := evaluationChange(t, s, duplicate)
		change.IdempotencyKey = "same-job-different-trial"
		if err := s.store.Transact(s.ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("external job counted twice: %v", err)
		}
	})
}
