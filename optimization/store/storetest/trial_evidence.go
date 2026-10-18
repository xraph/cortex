package storetest

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/run"
)

func trialEvidence(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("CapturedEvidenceCannotChangeIdentity", func(t *testing.T) {
		scenario := runningTrial(t, factory)
		original := capturedExecution(t, scenario.bundle, scenario.receipt)
		for _, mode := range []string{"output", "input", "prepared", "environment", "incomplete"} {
			t.Run(mode, func(t *testing.T) {
				record := original
				switch mode {
				case "output":
					record.Outcome.Output = "Tampered after sealing"
				case "input":
					record.InputDigest = strings.Repeat("e", 64)
				case "prepared":
					record.PreparedDigest = strings.Repeat("e", 64)
				case "environment":
					record.Environment.EnvironmentID = "different-environment"
				case "incomplete":
					record.State = run.StateIncomplete
					record.Outcome.StopReason = run.StopStepLimit
				}
				if mode != "output" {
					var sealErr error
					record, sealErr = optimization.SealExecution(record)
					if sealErr != nil {
						t.Fatal(sealErr)
					}
				}
				update := scenario.update(t, optimization.TrialCompleted)
				update.Execution = &record
				change := scenario.change(t, "bad-"+mode, update)
				if err := scenario.store.Transact(scenario.ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrIncompleteEvidence) {
					t.Fatalf("captured identity changed: %v", err)
				}
			})
		}
		update := scenario.update(t, optimization.TrialCompleted)
		update.Execution = &original
		commitTrialChange(scenario.ctx, t, scenario.store, scenario.change(t, "original-result", update))
	})
	t.Run("FailedAttemptRetainsExecutionAfterRetry", func(t *testing.T) {
		scenario := runningTrial(t, factory)
		record := capturedExecution(t, scenario.bundle, scenario.receipt)
		record.State = run.StateFailed
		record.Outcome.StopReason = run.StopExecutionError
		var err error
		record, err = optimization.SealExecution(record)
		if err != nil {
			t.Fatal(err)
		}
		update := scenario.update(t, optimization.TrialFailed)
		update.Execution = &record
		update.Error = "provider failed"
		commitTrialChange(scenario.ctx, t, scenario.store, scenario.change(t, "failed-result", update))
		unknown := scenario.update(t, optimization.TrialUnknown)
		unknown.Error = "late timeout"
		change := scenario.change(t, "late-unknown", unknown)
		if err = scenario.store.Transact(scenario.ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("known failed result became unknown: %v", err)
		}
		stop := optimization.StopReceipt{EnvironmentReceiptID: scenario.receipt.ID, AttemptID: scenario.receipt.AttemptID, Fence: scenario.receipt.Fence, State: optimization.StopQuarantined, Evidence: optimization.ContentRef{Ref: "quarantine", Revision: "1", Digest: strings.Repeat("d", 64)}}
		update = scenario.update(t, optimization.TrialFailed)
		update.Stop = &stop
		update.Error = "provider failed and environment quarantined"
		commitTrialChange(scenario.ctx, t, scenario.store, scenario.change(t, "quarantined", update))
		current, err := scenario.store.GetTrial(scenario.ctx, scenario.trial.ID)
		if err != nil {
			t.Fatal(err)
		}
		retry := optimization.TrialUpdate{TrialID: current.ID, ExpectedRevision: current.Revision, State: optimization.TrialRunning, StartAttemptID: id.New(optimization.PrefixAttempt), RetryApproval: &optimization.ContentRef{Ref: "operator-approved", Revision: "1", Digest: strings.Repeat("c", 64)}}
		commitTrialChange(scenario.ctx, t, scenario.store, scenario.change(t, "retry-failed", retry))
		current, err = scenario.store.GetTrial(scenario.ctx, current.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(current.Attempts) != 2 || current.Attempts[0].Execution == nil || current.Attempts[0].Execution.Digest != record.Digest || current.Execution != nil {
			t.Fatal("retry discarded failed attempt evidence")
		}
	})
	t.Run("OriginalRunCannotBackTwoRepetitions", func(t *testing.T) {
		scenario := runningTrial(t, factory)
		record := capturedExecution(t, scenario.bundle, scenario.receipt)
		update := scenario.update(t, optimization.TrialCompleted)
		update.Execution = &record
		commitTrialChange(scenario.ctx, t, scenario.store, scenario.change(t, "first-result", update))
		other := queuedTrial(scenario.bundle, 0)
		other.Repetition = 1
		receipt := openTrial(scenario.ctx, t, scenario.store, scenario.bundle, scenario.lease, other)
		replayed := record
		replayed.Environment = receipt
		var err error
		replayed, err = optimization.SealExecution(replayed)
		if err != nil {
			t.Fatal(err)
		}
		update = optimization.TrialUpdate{TrialID: other.ID, ExpectedRevision: 3, State: optimization.TrialCompleted, AttemptID: receipt.AttemptID, ExpectedAttemptRevision: 2, Execution: &replayed}
		change := scenario.change(t, "replayed-result", update)
		if err = scenario.store.Transact(scenario.ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("one run counted as two repetitions: %v", err)
		}
	})
	t.Run("TrialTakeoverFencesOldWorker", func(t *testing.T) {
		var nanos atomic.Int64
		nanos.Store(time.Now().UnixNano())
		clockFactory := func(t *testing.T, _ func() time.Time) store.Store {
			return factory(t, func() time.Time { return time.Unix(0, nanos.Load()).UTC() })
		}
		scenario := runningTrial(t, clockFactory)
		nanos.Add(int64(6 * time.Minute))
		var replacement optimization.WorkLease
		if err := scenario.store.Transact(scenario.ctx, func(tx store.Tx) error {
			var err error
			replacement, err = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: scenario.bundle.Experiment.ID, Owner: "replacement", Duration: time.Minute})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		update := scenario.update(t, optimization.TrialUnknown)
		update.Error = "original worker lost"
		change := scenario.change(t, "stale-update", update)
		if err := scenario.store.Transact(scenario.ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("stale worker changed attempt: %v", err)
		}
		change.Lease = replacement
		change.IdempotencyKey = "replacement-update"
		commitTrialChange(scenario.ctx, t, scenario.store, change)
		// Recovery retains the original environment receipt and original result.
		scenario.lease = replacement
		record := capturedExecution(t, scenario.bundle, scenario.receipt)
		update = scenario.update(t, optimization.TrialCompleted)
		update.Execution = &record
		commitTrialChange(scenario.ctx, t, scenario.store, scenario.change(t, "recovered-result", update))
	})
	t.Run("TrialConcurrencyIncludesUnknownEffects", func(t *testing.T) {
		scenario := runningTrial(t, factory)
		unknown := scenario.update(t, optimization.TrialUnknown)
		unknown.Error = "provider disconnected"
		commitTrialChange(scenario.ctx, t, scenario.store, scenario.change(t, "unknown", unknown))
		second := queuedTrial(scenario.bundle, 0)
		second.Repetition = 1
		openTrial(scenario.ctx, t, scenario.store, scenario.bundle, scenario.lease, second)
		third := queuedTrial(scenario.bundle, 0)
		third.Repetition = 2
		commitTrialChange(scenario.ctx, t, scenario.store, trialTransition(scenario.ctx, t, scenario.store, scenario.bundle.Experiment.ID, scenario.lease, "third-queued", []optimization.Trial{third}, nil))
		update := optimization.TrialUpdate{TrialID: third.ID, ExpectedRevision: 1, State: optimization.TrialRunning, StartAttemptID: id.New(optimization.PrefixAttempt)}
		change := scenario.change(t, "third-start", update)
		if err := scenario.store.Transact(scenario.ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("unknown external effect freed concurrency slot: %v", err)
		}
	})
}

type trialScenario struct {
	store   store.Store
	ctx     context.Context
	bundle  optimization.ExperimentBundle
	lease   optimization.WorkLease
	trial   optimization.Trial
	receipt optimization.EnvironmentReceipt
}

func runningTrial(t *testing.T, factory Factory) trialScenario {
	t.Helper()
	s, ctx, bundle, lease := trialFixture(t, factory)
	trial := queuedTrial(bundle, 0)
	receipt := openTrial(ctx, t, s, bundle, lease, trial)
	return trialScenario{store: s, ctx: ctx, bundle: bundle, lease: lease, trial: trial, receipt: receipt}
}

func openTrial(ctx context.Context, t *testing.T, s store.Store, bundle optimization.ExperimentBundle, lease optimization.WorkLease, trial optimization.Trial) optimization.EnvironmentReceipt {
	t.Helper()
	key := trial.ID.String()
	commitTrialChange(ctx, t, s, trialTransition(ctx, t, s, bundle.Experiment.ID, lease, key+"/queue", []optimization.Trial{trial}, nil))
	attemptID := id.New(optimization.PrefixAttempt)
	commitTrialChange(ctx, t, s, trialTransition(ctx, t, s, bundle.Experiment.ID, lease, key+"/start", nil, []optimization.TrialUpdate{{TrialID: trial.ID, ExpectedRevision: 1, State: optimization.TrialRunning, StartAttemptID: attemptID}}))
	receipt := trialReceipt(bundle, lease, attemptID)
	for _, task := range bundle.Experiment.Spec.Tasks {
		if task.ID == trial.CaseID {
			receipt.Snapshot = task.EnvironmentSnapshot
		}
	}
	commitTrialChange(ctx, t, s, trialTransition(ctx, t, s, bundle.Experiment.ID, lease, key+"/receipt", nil, []optimization.TrialUpdate{{TrialID: trial.ID, ExpectedRevision: 2, State: optimization.TrialRunning, AttemptID: attemptID, ExpectedAttemptRevision: 1, Receipt: &receipt}}))
	return receipt
}

func (s trialScenario) update(t *testing.T, state optimization.TrialState) optimization.TrialUpdate {
	t.Helper()
	trial, err := s.store.GetTrial(s.ctx, s.trial.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempt := trial.Attempts[len(trial.Attempts)-1]
	return optimization.TrialUpdate{TrialID: trial.ID, ExpectedRevision: trial.Revision, State: state, AttemptID: attempt.ID, ExpectedAttemptRevision: attempt.Revision}
}

func (s trialScenario) change(t *testing.T, key string, update optimization.TrialUpdate) optimization.Transition {
	t.Helper()
	return trialTransition(s.ctx, t, s.store, s.bundle.Experiment.ID, s.lease, key, nil, []optimization.TrialUpdate{update})
}
