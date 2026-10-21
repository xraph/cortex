package state

import (
	"strings"

	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/run"
)

func (tx *transaction) applyTrialUpdate(experiment optimization.Experiment, trial *optimization.Trial, update optimization.TrialUpdate, lease optimization.WorkLease, pending map[string]optimization.Trial) error {
	if !update.StartAttemptID.IsNil() {
		return tx.startAttempt(experiment, trial, update, lease, pending)
	}
	if update.RetryApproval != nil {
		return optimization.ErrInvalidSpec
	}
	if len(trial.Attempts) == 0 {
		if trial.State != optimization.TrialQueued || !update.AttemptID.IsNil() || update.ExpectedAttemptRevision != 0 || update.Receipt != nil || update.Stop != nil || update.Execution != nil || (update.State != optimization.TrialBlocked && update.State != optimization.TrialCancelled) || strings.TrimSpace(update.Error) == "" {
			return optimization.ErrConflict
		}
		trial.State = update.State
		trial.Error = update.Error
		return nil
	}
	if trial.State == optimization.TrialCompleted || trial.State == optimization.TrialInvalidated {
		return optimization.ErrConflict
	}
	attempt := &trial.Attempts[len(trial.Attempts)-1]
	if attempt.ID != update.AttemptID || attempt.Revision != update.ExpectedAttemptRevision {
		return optimization.ErrConflict
	}
	priorReceipt := attempt.Receipt
	if attempt.Execution != nil && (update.State != attempt.State || update.Execution != nil || update.Receipt != nil || update.Stop == nil) {
		return optimization.ErrConflict
	}
	if update.Execution != nil && priorReceipt == nil {
		return optimization.ErrIncompleteEvidence
	}
	if update.Receipt != nil {
		if err := tx.attachReceipt(experiment, *trial, attempt, *update.Receipt, pending); err != nil {
			return err
		}
	}
	if update.Stop != nil {
		if priorReceipt == nil {
			return optimization.ErrIncompleteEvidence
		}
		stop := *update.Stop
		if err := stop.Validate(); err != nil {
			return err
		}
		if stop.AttemptID != attempt.ID || stop.EnvironmentReceiptID != priorReceipt.ID || stop.Fence != (optimization.Fence{Owner: lease.Owner, Generation: lease.Generation}) {
			return optimization.ErrConflict
		}
		if confirmedStop(attempt.Stop) && !same(attempt.Stop, update.Stop) {
			return optimization.ErrConflict
		}
		stop.ObservedAt = tx.now
		attempt.Stop = &stop
	}
	if update.Execution != nil {
		if err := tx.attachExecution(experiment, trial, attempt, *update.Execution, update.State, pending); err != nil {
			return err
		}
	} else {
		switch update.State {
		case optimization.TrialRunning:
			if trial.State != optimization.TrialRunning || update.Receipt == nil || update.Stop != nil || update.Error != "" {
				return optimization.ErrConflict
			}
		case optimization.TrialUnknown:
			if strings.TrimSpace(update.Error) == "" {
				return optimization.ErrInvalidSpec
			}
		case optimization.TrialFailed, optimization.TrialCancelled, optimization.TrialBlocked:
			if !confirmedStop(attempt.Stop) || strings.TrimSpace(update.Error) == "" {
				return optimization.ErrUnknownAttempt
			}
			attempt.CompletedAt = tx.now
		default:
			return optimization.ErrConflict
		}
	}
	attempt.State = update.State
	attempt.Error = update.Error
	attempt.Revision++
	trial.State = update.State
	trial.Error = update.Error
	return nil
}

func (tx *transaction) startAttempt(experiment optimization.Experiment, trial *optimization.Trial, update optimization.TrialUpdate, lease optimization.WorkLease, pending map[string]optimization.Trial) error {
	if trial.EvaluationID != "" {
		return optimization.ErrConflict
	}
	if update.State != optimization.TrialRunning || !update.AttemptID.IsNil() || update.ExpectedAttemptRevision != 0 || update.Receipt != nil || update.Stop != nil || update.Execution != nil || update.Error != "" {
		return optimization.ErrInvalidSpec
	}
	if _, err := optimization.ParseID(update.StartAttemptID.String(), optimization.PrefixAttempt); err != nil {
		return err
	}
	if trial.Split == optimization.SplitValidation {
		if err := tx.validateTrialAllocation(experiment, *trial, true); err != nil {
			return err
		}
	}
	if tx.anyTrial(pending, func(value optimization.Trial) bool {
		for _, attempt := range value.Attempts {
			if attempt.ID == update.StartAttemptID {
				return true
			}
		}
		return false
	}) {
		return optimization.ErrConflict
	}
	active := 0
	tx.anyTrial(pending, func(value optimization.Trial) bool {
		if value.ExperimentID == experiment.ID {
			for _, prior := range value.Attempts {
				if prior.Execution == nil && !confirmedStop(prior.Stop) {
					active++
				}
			}
		}
		return false
	})
	if active >= experiment.Spec.Limits.Concurrency {
		return optimization.ErrConflict
	}
	tokens, money := debits(tx.data.Reservations[experiment.ID.String()])
	if tokens > experiment.Spec.Budget.MaxTokens || (!experiment.Spec.Budget.TokenOnly && money > experiment.Spec.Budget.MaxUSDMicros) {
		return optimization.ErrBudgetExceeded
	}
	attempt := optimization.Attempt{ID: update.StartAttemptID, State: optimization.TrialRunning, Revision: 1, Fence: optimization.Fence{Owner: lease.Owner, Generation: lease.Generation}, StartedAt: tx.now}
	if len(trial.Attempts) == 0 {
		if trial.State != optimization.TrialQueued || update.RetryApproval != nil {
			return optimization.ErrConflict
		}
	} else {
		if trial.State != optimization.TrialUnknown && trial.State != optimization.TrialFailed && trial.State != optimization.TrialCancelled {
			return optimization.ErrConflict
		}
		previous := trial.Attempts[len(trial.Attempts)-1]
		if previous.Receipt == nil || !confirmedStop(previous.Stop) {
			return optimization.ErrUnknownAttempt
		}
		if !previous.Receipt.Capabilities.Resettable && !previous.Receipt.Capabilities.IdempotentEffects {
			return optimization.ErrPolicyDenied
		}
		if update.RetryApproval == nil {
			return optimization.ErrPolicyDenied
		}
		if err := update.RetryApproval.Validate(); err != nil {
			return err
		}
		approval := *update.RetryApproval
		attempt.RetryApproval = &approval
		attempt.Replaces = previous.ID
	}
	trial.Attempts = append(trial.Attempts, attempt)
	trial.State = optimization.TrialRunning
	trial.Execution = nil
	trial.Error = ""
	return nil
}

func (tx *transaction) attachReceipt(experiment optimization.Experiment, trial optimization.Trial, attempt *optimization.Attempt, receipt optimization.EnvironmentReceipt, pending map[string]optimization.Trial) error {
	if err := receipt.Validate(); err != nil {
		return err
	}
	task, exists := trialTask(experiment, trial)
	if !exists {
		return optimization.ErrInvalidSpec
	}
	scope, err := optimization.ScopeKey(receipt.Scope)
	if err != nil {
		return err
	}
	if scope != tx.scope || receipt.AttemptID != attempt.ID || receipt.Snapshot != task.EnvironmentSnapshot || receipt.Fence != attempt.Fence {
		return optimization.ErrPolicyDenied
	}
	if attempt.Receipt != nil {
		if !same(attempt.Receipt, &receipt) {
			return optimization.ErrConflict
		}
		return nil
	}
	if tx.anyTrial(pending, func(value optimization.Trial) bool {
		for _, prior := range value.Attempts {
			if prior.Receipt != nil && prior.Receipt.ID == receipt.ID {
				return true
			}
		}
		return false
	}) {
		return optimization.ErrConflict
	}
	detached, err := clone(receipt)
	if err != nil {
		return err
	}
	attempt.Receipt = &detached
	return nil
}

func (tx *transaction) attachExecution(experiment optimization.Experiment, trial *optimization.Trial, attempt *optimization.Attempt, record optimization.ExecutionRecord, state optimization.TrialState, pending map[string]optimization.Trial) error {
	if tx.original == nil {
		return optimization.ErrIncompleteEvidence
	}
	committed, exists := tx.original.Trials[trial.ID.String()]
	if !exists {
		return optimization.ErrIncompleteEvidence
	}
	receiptCommitted := false
	for _, prior := range committed.Attempts {
		if prior.ID == attempt.ID && prior.Receipt != nil && same(prior.Receipt, attempt.Receipt) {
			receiptCommitted = true
			break
		}
	}
	if !receiptCommitted {
		return optimization.ErrIncompleteEvidence
	}
	sealed, err := optimization.SealExecution(record)
	if err != nil {
		return err
	}
	task, exists := trialTask(experiment, *trial)
	if !exists {
		return optimization.ErrInvalidSpec
	}
	version := tx.data.Versions[trial.Version.ID.String()]
	if sealed.Digest != record.Digest || record.InputDigest != task.InputDigest || version.Prepared == nil || record.PreparedDigest != version.Prepared.Digest() || !same(attempt.Receipt, &record.Environment) {
		return optimization.ErrIncompleteEvidence
	}
	expected := optimization.TrialFailed
	switch record.State {
	case run.StateCompleted:
		expected = optimization.TrialCompleted
	case run.StateCancelled:
		expected = optimization.TrialCancelled
	}
	if state != expected {
		return optimization.ErrIncompleteEvidence
	}
	if tx.anyTrial(pending, func(value optimization.Trial) bool {
		for _, prior := range value.Attempts {
			if prior.Execution != nil && prior.Execution.RunID == record.RunID {
				return true
			}
		}
		return false
	}) {
		return optimization.ErrConflict
	}
	trial.Execution = &sealed
	attempt.Execution = &sealed
	attempt.Usage = record.Usage
	attempt.CompletedAt = record.CompletedAt
	return nil
}

func (tx *transaction) anyTrial(pending map[string]optimization.Trial, predicate func(optimization.Trial) bool) bool {
	for _, trial := range pending {
		if predicate(trial) {
			return true
		}
	}
	for key, trial := range tx.data.Trials {
		if _, replaced := pending[key]; !replaced && predicate(trial) {
			return true
		}
	}
	return false
}

func confirmedStop(stop *optimization.StopReceipt) bool {
	return stop != nil && (stop.State == optimization.StopTerminated || stop.State == optimization.StopQuarantined)
}
