package storetest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/run"
)

func trials(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("TrialIdentityAndRollback", func(t *testing.T) {
		s, ctx, bundle, lease := trialFixture(t, factory)
		trial := queuedTrial(bundle, 0)
		change := trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "queue", []optimization.Trial{trial}, nil)
		abort := errors.New("abort trial")
		if err := s.Transact(ctx, func(tx store.Tx) error {
			if err := tx.CommitTransition(change); err != nil {
				return err
			}
			return abort
		}); !errors.Is(err, abort) {
			t.Fatal(err)
		}
		if _, err := s.GetTrial(ctx, trial.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("trial escaped rollback: %v", err)
		}
		for range 2 {
			if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); err != nil {
				t.Fatal(err)
			}
		}
		stored, err := s.GetTrial(ctx, trial.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Revision != 1 || stored.State != optimization.TrialQueued || stored.CreatedAt.IsZero() || stored.CaseDigest != trial.CaseDigest {
			t.Fatalf("trial identity changed: %+v", stored)
		}
		duplicate := trial
		duplicate.ID = id.New(optimization.PrefixTrial)
		change = trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "duplicate", []optimization.Trial{duplicate}, nil)
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("case/version/repetition duplicated: %v", err)
		}
		foreign := scoped(Fixture(t, "foreign-trial").Experiment.Spec.Scope)
		if _, err = s.GetTrial(foreign, trial.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("trial escaped scope: %v", err)
		}
	})
	t.Run("TrialRejectsUnfrozenIdentity", func(t *testing.T) {
		for name, mutate := range map[string]func(*optimization.Trial){
			"case":       func(x *optimization.Trial) { x.CaseDigest = strings.Repeat("f", 64) },
			"version":    func(x *optimization.Trial) { x.Version.Digest = strings.Repeat("f", 64) },
			"experiment": func(x *optimization.Trial) { x.ExperimentID = id.New(optimization.PrefixExperiment) },
			"candidate":  func(x *optimization.Trial) { x.CandidateID = id.New(optimization.PrefixCandidate) },
			"repetition": func(x *optimization.Trial) { x.Repetition = 3 },
			"state":      func(x *optimization.Trial) { x.State = optimization.TrialCompleted },
			"revision":   func(x *optimization.Trial) { x.Revision = 9 },
		} {
			t.Run(name, func(t *testing.T) {
				s, ctx, bundle, lease := trialFixture(t, factory)
				trial := queuedTrial(bundle, 0)
				mutate(&trial)
				change := trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "bad-trial", []optimization.Trial{trial}, nil)
				if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); err == nil {
					t.Fatal("unfrozen trial admitted")
				}
			})
		}
	})
	t.Run("ReceiptPrecedesCapturedExecution", func(t *testing.T) {
		s, ctx, bundle, lease := trialFixture(t, factory)
		trial := queuedTrial(bundle, 0)
		commitTrialChange(ctx, t, s, trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "queue", []optimization.Trial{trial}, nil))
		attemptID := id.New(optimization.PrefixAttempt)
		start := optimization.TrialUpdate{TrialID: trial.ID, ExpectedRevision: 1, State: optimization.TrialRunning, StartAttemptID: attemptID}
		commitTrialChange(ctx, t, s, trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "attempt-start", nil, []optimization.TrialUpdate{start}))
		receipt := trialReceipt(bundle, lease, attemptID)
		record := capturedExecution(t, bundle, receipt)
		finish := optimization.TrialUpdate{TrialID: trial.ID, ExpectedRevision: 2, State: optimization.TrialCompleted, AttemptID: attemptID, ExpectedAttemptRevision: 1, Execution: &record}
		change := trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "premature-result", nil, []optimization.TrialUpdate{finish})
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("execution accepted without persisted receipt: %v", err)
		}
		finish.Receipt = &receipt
		change = trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "same-write-receipt-result", nil, []optimization.TrialUpdate{finish})
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("receipt first persisted after execution: %v", err)
		}
		attach := optimization.TrialUpdate{TrialID: trial.ID, ExpectedRevision: 2, State: optimization.TrialRunning, AttemptID: attemptID, ExpectedAttemptRevision: 1, Receipt: &receipt}
		attachChange := trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "uncommitted-receipt", nil, []optimization.TrialUpdate{attach})
		finishInTransaction := finish
		finishInTransaction.Receipt = nil
		finishInTransaction.ExpectedRevision = 3
		finishInTransaction.ExpectedAttemptRevision = 2
		finishChange := attachChange
		finishChange.ExpectedRevision++
		finishChange.IdempotencyKey = "uncommitted-result"
		finishChange.TrialUpdates = []optimization.TrialUpdate{finishInTransaction}
		if err := s.Transact(ctx, func(tx store.Tx) error {
			if err := tx.CommitTransition(attachChange); err != nil {
				return err
			}
			return tx.CommitTransition(finishChange)
		}); !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("receipt and result committed in one transaction: %v", err)
		}
		commitTrialChange(ctx, t, s, trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "receipt", nil, []optimization.TrialUpdate{attach}))
		finish.Receipt = nil
		finish.ExpectedRevision = 3
		finish.ExpectedAttemptRevision = 2
		change = trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "result", nil, []optimization.TrialUpdate{finish})
		commitTrialChange(ctx, t, s, change)
		commitTrialChange(ctx, t, s, change)
		stored, err := s.GetTrial(ctx, trial.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.State != optimization.TrialCompleted || stored.Execution.Outcome.Output != "Captured answer" || stored.Execution.Digest != record.Digest || len(stored.Attempts) != 1 || stored.Attempts[0].CompletedAt.IsZero() {
			t.Fatalf("captured execution lost: %+v", stored)
		}
		stored.Execution.Outcome.Output = "changed"
		stored.Attempts[0].Receipt.EnvironmentID = "changed"
		fresh, err := s.GetTrial(ctx, trial.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.Execution.Outcome.Output != "Captured answer" || fresh.Attempts[0].Receipt.EnvironmentID != receipt.EnvironmentID {
			t.Fatal("trial read shares mutable evidence")
		}
	})
	t.Run("UnknownAttemptRequiresContainmentAndApproval", func(t *testing.T) {
		s, ctx, bundle, lease := trialFixture(t, factory)
		trial := queuedTrial(bundle, 0)
		commitTrialChange(ctx, t, s, trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "queue", []optimization.Trial{trial}, nil))
		attemptID := id.New(optimization.PrefixAttempt)
		commitTrialChange(ctx, t, s, trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "attempt-start", nil, []optimization.TrialUpdate{{TrialID: trial.ID, ExpectedRevision: 1, State: optimization.TrialRunning, StartAttemptID: attemptID}}))
		receipt := trialReceipt(bundle, lease, attemptID)
		commitTrialChange(ctx, t, s, trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "receipt", nil, []optimization.TrialUpdate{{TrialID: trial.ID, ExpectedRevision: 2, State: optimization.TrialRunning, AttemptID: attemptID, ExpectedAttemptRevision: 1, Receipt: &receipt}}))
		commitTrialChange(ctx, t, s, trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "unknown", nil, []optimization.TrialUpdate{{TrialID: trial.ID, ExpectedRevision: 3, State: optimization.TrialUnknown, AttemptID: attemptID, ExpectedAttemptRevision: 2, Error: "connection lost"}}))
		approval := optimization.ContentRef{Ref: "approval/operator", Revision: "1", Digest: strings.Repeat("c", 64)}
		retry := optimization.TrialUpdate{TrialID: trial.ID, ExpectedRevision: 4, State: optimization.TrialRunning, StartAttemptID: id.New(optimization.PrefixAttempt), RetryApproval: &approval}
		change := trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "unsafe-retry", nil, []optimization.TrialUpdate{retry})
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrUnknownAttempt) {
			t.Fatalf("uncertain external effect silently repeated: %v", err)
		}
		stop := optimization.StopReceipt{EnvironmentReceiptID: receipt.ID, AttemptID: attemptID, Fence: receipt.Fence, State: optimization.StopTerminated, Evidence: optimization.ContentRef{Ref: "provider/stopped", Revision: "1", Digest: strings.Repeat("d", 64)}}
		commitTrialChange(ctx, t, s, trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "stop-confirmed", nil, []optimization.TrialUpdate{{TrialID: trial.ID, ExpectedRevision: 4, State: optimization.TrialUnknown, AttemptID: attemptID, ExpectedAttemptRevision: 3, Stop: &stop, Error: "result unavailable after termination"}}))
		retry.ExpectedRevision = 5
		retry.RetryApproval = nil
		change = trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "unapproved-retry", nil, []optimization.TrialUpdate{retry})
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("retry without operator approval accepted: %v", err)
		}
		retry.RetryApproval = &approval
		commitTrialChange(ctx, t, s, trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "approved-retry", nil, []optimization.TrialUpdate{retry}))
		stored, err := s.GetTrial(ctx, trial.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(stored.Attempts) != 2 || stored.Attempts[0].ID != attemptID || stored.Attempts[0].Stop.State != optimization.StopTerminated || stored.Attempts[1].Replaces != attemptID || stored.Attempts[1].RetryApproval.Ref != approval.Ref {
			t.Fatalf("replacement erased prior attempt: %+v", stored.Attempts)
		}
	})
	t.Run("ValidationExposureBeforeAttempt", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := Fixture(t, "trial-validation")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		lease := validationExperiment(ctx, t, s, bundle)
		allocation := allocate(ctx, t, s, allocationRequest(bundle, lease))
		trial := queuedTrial(bundle, 1)
		trial.AllocationID = allocation.ID
		commitTrialChange(ctx, t, s, trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "queue-validation", []optimization.Trial{trial}, nil))
		start := optimization.TrialUpdate{TrialID: trial.ID, ExpectedRevision: 1, State: optimization.TrialRunning, StartAttemptID: id.New(optimization.PrefixAttempt)}
		change := trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "start-validation", nil, []optimization.TrialUpdate{start})
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("validation dispatched before exposure: %v", err)
		}
		if err := s.Transact(ctx, func(tx store.Tx) error {
			return tx.ExposeValidation(optimization.ValidationAccess{AllocationID: allocation.ID, ExperimentID: bundle.Experiment.ID, Lease: lease})
		}); err != nil {
			t.Fatal(err)
		}
		commitTrialChange(ctx, t, s, change)
	})
}

func trialFixture(t *testing.T, factory Factory) (store.Store, context.Context, optimization.ExperimentBundle, optimization.WorkLease) {
	t.Helper()
	s := factory(t, time.Now)
	bundle := Fixture(t, "trial")
	ctx := scoped(bundle.Experiment.Spec.Scope)
	var lease optimization.WorkLease
	if err := s.Transact(ctx, func(tx store.Tx) error {
		var err error
		lease, err = startFixture(tx, bundle, "trial-worker")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return s, ctx, bundle, lease
}

func startFixture(tx store.Tx, bundle optimization.ExperimentBundle, owner string) (optimization.WorkLease, error) {
	if err := tx.CreateExperiment(bundle); err != nil {
		return optimization.WorkLease{}, err
	}
	if err := tx.CommitTransition(optimization.Transition{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 1, NextState: optimization.StateQueued, IdempotencyKey: "start", Actor: bundle.Experiment.Spec.Requester}); err != nil {
		return optimization.WorkLease{}, err
	}
	return tx.ClaimWork(optimization.ClaimRequest{ExperimentID: bundle.Experiment.ID, Owner: owner, Duration: 5 * time.Minute})
}

func queuedTrial(bundle optimization.ExperimentBundle, taskIndex int) optimization.Trial {
	task := bundle.Experiment.Spec.Tasks[taskIndex]
	return optimization.Trial{ID: id.New(optimization.PrefixTrial), ExperimentID: bundle.Experiment.ID, Version: bundle.Experiment.Spec.Baseline, CaseID: task.ID, CaseDigest: task.Digest}
}

func trialTransition(ctx context.Context, t *testing.T, s store.Store, experimentID id.ID, lease optimization.WorkLease, key string, records []optimization.Trial, updates []optimization.TrialUpdate) optimization.Transition {
	t.Helper()
	experiment, err := s.GetExperiment(ctx, experimentID)
	if err != nil {
		t.Fatal(err)
	}
	return optimization.Transition{ExperimentID: experimentID, ExpectedRevision: experiment.Revision, NextState: optimization.StateRunning, NextPhase: experiment.Phase, Lease: lease, IdempotencyKey: key, Actor: experiment.Spec.Requester, Trials: records, TrialUpdates: updates}
}

func commitTrialChange(ctx context.Context, t *testing.T, s store.Store, change optimization.Transition) {
	t.Helper()
	if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); err != nil {
		t.Fatal(err)
	}
}

func trialReceipt(bundle optimization.ExperimentBundle, lease optimization.WorkLease, attemptID id.ID) optimization.EnvironmentReceipt {
	return optimization.EnvironmentReceipt{ID: "receipt/" + attemptID.String(), Provider: "fixture", EnvironmentID: "environment/" + attemptID.String(), Scope: bundle.Experiment.Spec.Scope, AttemptID: attemptID, Snapshot: bundle.Experiment.Spec.Tasks[0].EnvironmentSnapshot, Capabilities: optimization.IsolationCapabilities{Isolated: true, Resettable: true, Recoverable: true, Stoppable: true}, Fence: optimization.Fence{Owner: lease.Owner, Generation: lease.Generation}}
}

func capturedExecution(t *testing.T, bundle optimization.ExperimentBundle, receipt optimization.EnvironmentReceipt) optimization.ExecutionRecord {
	t.Helper()
	start := time.Now().Add(-time.Second).UTC()
	end := start.Add(time.Millisecond)
	record, err := optimization.SealExecution(optimization.ExecutionRecord{RunID: id.NewAgentRunID(), PreparedDigest: bundle.Versions[0].Prepared.Digest(), InputDigest: bundle.Experiment.Spec.Tasks[0].InputDigest, Outcome: run.Outcome{StopReason: run.StopFinalAnswer, Output: "Captured answer", FinishReason: "stop", StepCount: 1, TokensUsed: 10, CompletedAt: end}, State: run.StateCompleted, Usage: optimization.Usage{Tokens: 10, TokenQuality: optimization.UsageKnown}, StartedAt: start, CompletedAt: end, Duration: time.Millisecond, Environment: receipt, Artifacts: []optimization.ArtifactRef{}})
	if err != nil {
		t.Fatal(err)
	}
	return record
}
