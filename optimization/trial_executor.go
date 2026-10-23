package optimization

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization/internal/canonical"
	"github.com/xraph/cortex/run"
)

// TrialAllowance is a host-approved bound for every charged operation in a trial.
// The environment must enforce these limits, including model, tool and retrieval work.
type TrialAllowance struct {
	MaxTokens       int64  `json:"max_tokens"`
	MaxUSDMicros    int64  `json:"max_usd_micros"`
	PricingRevision string `json:"pricing_revision"`
}

// TrialConfig binds trial execution to host isolation, accounting and recorded scoring.
type TrialConfig struct {
	Repository       Repository
	Authorizer       Authorizer
	IdentityResolver IdentityResolver
	Environments     EnvironmentFactory
	Allowance        TrialAllowance
	Evaluator        Evaluator
}

// TrialPhaseHandler advances durable execution and captured evaluation steps.
type TrialPhaseHandler struct {
	config TrialConfig
	meter  *LedgerMeter
}

// NewTrialPhaseHandler installs host adapters without starting any work.
func NewTrialPhaseHandler(config TrialConfig) (*TrialPhaseHandler, error) {
	if config.Environments == nil || config.Evaluator == nil || config.Allowance.MaxTokens <= 0 || config.Allowance.MaxUSDMicros < 0 {
		return nil, ErrInvalidSpec
	}
	meter, err := NewLedgerMeter(config.Repository, config.Authorizer, config.IdentityResolver)
	if err != nil {
		return nil, err
	}
	return &TrialPhaseHandler{config: config, meter: meter}, nil
}

// Execute performs at most one trial operation or returns a phase transition.
func (h *TrialPhaseHandler) Execute(ctx context.Context, item WorkItem) (Transition, error) {
	if item.Experiment.Phase != PhaseBaseline && item.Experiment.Phase != PhaseProbe && item.Experiment.Phase != PhaseValidate && item.Experiment.Phase != PhaseDecide {
		return Transition{}, ErrUnavailable
	}
	resolved, done, err := h.meter.authorize(ctx, item.Experiment.ID)
	if err != nil {
		return Transition{}, err
	}
	defer done()
	current, err := h.config.Repository.GetExperiment(resolved, item.Experiment.ID)
	if err != nil {
		return Transition{}, err
	}
	if current.State != StateRunning || current.Revision != item.Experiment.Revision || current.Phase != item.Experiment.Phase || current.Lease.Owner != item.Lease.Owner || current.Lease.Generation != item.Lease.Generation || item.Lease.ExperimentID != current.ID || !time.Now().Before(current.Lease.ExpiresAt) || !time.Now().Before(current.Deadline) {
		return Transition{}, ErrConflict
	}
	item.Experiment = current
	if current.Spec.Mode != ModeSequential {
		return Transition{}, ErrUnavailable
	}
	if !current.Spec.Budget.TokenOnly && (h.config.Allowance.MaxUSDMicros <= 0 || h.config.Allowance.PricingRevision != current.Spec.Budget.PricingRevision) {
		return Transition{}, ErrInvalidSpec
	}
	trials, err := h.listTrials(ctx, item.Experiment.ID)
	if err != nil {
		return Transition{}, err
	}
	if item.Experiment.Phase == PhaseDecide {
		return h.decide(ctx, item, trials)
	}
	scheduled, err := h.schedule(ctx, item, trials)
	if err != nil {
		return Transition{}, err
	}
	if len(scheduled.Trials) > 0 {
		return scheduled, nil
	}

	for _, trial := range trials {
		if !trialInPhase(trial, item.Experiment.Phase) {
			continue
		}
		if trial.Execution != nil && trial.EvaluationID != "" || trial.Execution == nil && trial.Error != "" && (trial.State == TrialBlocked || trial.State == TrialFailed || trial.State == TrialCancelled) {
			continue
		}
		return h.step(ctx, item, trial)
	}
	if item.Experiment.Phase == PhaseBaseline {
		return Transition{NextState: StateRunning, NextPhase: PhasePropose}, nil
	}
	if item.Experiment.Phase == PhaseValidate {
		return Transition{NextState: StateRunning, NextPhase: PhaseDecide}, nil
	}
	return h.afterProbe(ctx, item.Experiment)
}

func trialInPhase(t Trial, p Phase) bool {
	switch p {
	case PhaseBaseline:
		return t.Split == SplitDiscovery && t.CandidateID.IsNil()
	case PhaseProbe:
		return t.Split == SplitDiscovery && !t.CandidateID.IsNil()
	case PhaseValidate:
		return t.Split == SplitValidation
	}
	return false
}
func (h *TrialPhaseHandler) listTrials(ctx context.Context, experiment id.ID) ([]Trial, error) {
	var out []Trial
	q := TrialQuery{PageQuery: PageQuery{Limit: 100}}
	for {
		page, err := h.config.Repository.ListTrials(ctx, experiment, q)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			return out, nil
		}
		q.Cursor = page.NextCursor
	}
}
func (h *TrialPhaseHandler) step(ctx context.Context, item WorkItem, trial Trial) (result Transition, retErr error) {
	change := Transition{NextState: StateRunning, NextPhase: item.Experiment.Phase}
	update := TrialUpdate{TrialID: trial.ID, ExpectedRevision: trial.Revision, State: TrialRunning}
	if len(trial.Attempts) == 0 {
		update.StartAttemptID = id.New(PrefixAttempt)
		change.TrialUpdates = []TrialUpdate{update}
		return change, nil
	}
	attempt := trial.Attempts[len(trial.Attempts)-1]
	update.AttemptID, update.ExpectedAttemptRevision = attempt.ID, attempt.Revision
	if trial.Execution != nil {
		if err := h.checkArtifacts(ctx, trial); err != nil {
			return Transition{}, err
		}
		if err := h.settle(ctx, item, trial, *trial.Execution); err != nil {
			return Transition{}, err
		}
		evaluation, err := h.config.Evaluator.Evaluate(ctx, EvaluationRequest{ExperimentID: trial.ExperimentID, TrialID: trial.ID, CandidateID: trial.CandidateID, CaseID: trial.CaseID, CaseDigest: trial.CaseDigest, Repetition: trial.Repetition, Plan: item.Experiment.EvaluationPlan, Execution: *trial.Execution, IdempotencyKey: "trial/" + trial.ID.String() + "/" + trial.Execution.Digest})
		if err != nil {
			return Transition{}, err
		}
		if validationErr := evaluation.ValidateCaptured(item.Experiment, trial); validationErr != nil {
			return Transition{}, validationErr
		}
		change.Evaluations = []Evaluation{evaluation}
		return change, nil
	}
	var task TaskCase
	for _, value := range item.Experiment.Spec.Tasks {
		if value.ID == trial.CaseID && value.Digest == trial.CaseDigest {
			task = value
		}
	}
	if task.ID == "" {
		return Transition{}, ErrIncompleteEvidence
	}
	deadline := attempt.StartedAt.Add(item.Experiment.Spec.Limits.TrialTimeout)
	if deadline.After(item.Experiment.Deadline) {
		deadline = item.Experiment.Deadline
	}
	request := TrialRequest{Scope: item.Experiment.Spec.Scope, ExperimentID: item.Experiment.ID, TrialID: trial.ID, AttemptID: attempt.ID, Task: task, Version: trial.Version, Fence: attempt.Fence, Deadline: deadline, Allowance: h.config.Allowance, Limits: item.Experiment.Spec.Limits}
	if attempt.Receipt == nil {
		if !time.Now().Before(deadline) {
			return Transition{}, ErrUnknownAttempt
		}
		openCtx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		environment, err := h.config.Environments.Open(openCtx, request)
		if err != nil {
			return Transition{}, err
		}
		if environment == nil {
			return Transition{}, ErrIncompleteEvidence
		}
		defer func() { retErr = errors.Join(retErr, h.closeEnvironment(ctx, environment)) }()
		receipt := environment.Receipt()
		if validationErr := receipt.Validate(); validationErr != nil {
			return Transition{}, validationErr
		}
		if receipt.AttemptID != attempt.ID || receipt.Scope.Canonical() != request.Scope.Canonical() || receipt.Snapshot != task.EnvironmentSnapshot || receipt.Fence != attempt.Fence {
			return Transition{}, ErrIncompleteEvidence
		}
		if validationErr := ValidateTrialCapabilities(receipt.Capabilities, item.Experiment.Spec.Objective.RequireReproducibility); validationErr != nil {
			return Transition{}, validationErr
		}
		update.Receipt = &receipt
		change.TrialUpdates = []TrialUpdate{update}
		return change, nil
	}
	if err := ValidateTrialCapabilities(attempt.Receipt.Capabilities, item.Experiment.Spec.Objective.RequireReproducibility); err != nil {
		return Transition{}, err
	}
	recoveryCtx, recoveryCancel := context.WithTimeout(ctx, 30*time.Second)
	defer recoveryCancel()
	environment, err := h.config.Environments.Recover(recoveryCtx, *attempt.Receipt)
	if err != nil {
		return Transition{}, err
	}
	if environment == nil {
		return Transition{}, ErrIncompleteEvidence
	}
	defer func() { retErr = errors.Join(retErr, h.closeEnvironment(ctx, environment)) }()
	if !equalControllerData(environment.Receipt(), *attempt.Receipt) {
		return Transition{}, ErrIncompleteEvidence
	}
	key := "trial/" + trial.ID.String() + "/" + attempt.ID.String()
	saved, lookupErr := h.config.Repository.GetReservation(ctx, item.Experiment.ID, key)
	var record *ExecutionRecord
	if lookupErr == nil && saved.Call != nil {
		status, inspectErr := environment.Inspect(recoveryCtx, attempt.ID)
		if inspectErr != nil {
			return Transition{}, errors.Join(inspectErr, h.stopAttempt(ctx, item, *attempt.Receipt))
		}
		if status.AttemptID != attempt.ID || status.Execution == nil {
			return Transition{}, errors.Join(ErrUnknownAttempt, h.stopAttempt(ctx, item, *attempt.Receipt))
		}
		record = status.Execution
	} else {
		if lookupErr != nil && !errors.Is(lookupErr, ErrNotFound) {
			return Transition{}, lookupErr
		}
		if !time.Now().Before(deadline) {
			return Transition{}, ErrUnknownAttempt
		}
		version, getErr := h.config.Repository.GetVersion(ctx, trial.Version.ID)
		if getErr != nil {
			return Transition{}, getErr
		}
		sealed, sealErr := SealVersion(version)
		if sealErr != nil || sealed.Digest != trial.Version.Digest || sealed.Prepared == nil {
			return Transition{}, ErrStaleVersion
		}
		reservation := UsageReservation{ExperimentID: item.Experiment.ID, AttemptID: attempt.ID, CallKey: key, Operation: trialOperation(item.Experiment.Phase), Lease: item.Lease, ExpiresAt: deadline, MaxTokens: h.config.Allowance.MaxTokens, MaxUSDMicros: h.config.Allowance.MaxUSDMicros, PricingRevision: h.config.Allowance.PricingRevision}
		encoded, encodeErr := canonical.Marshal(request)
		if encodeErr != nil {
			return Transition{}, encodeErr
		}
		if _, err = h.meter.BeginCall(ctx, reservation, digestBytes(encoded)); err != nil {
			return Transition{}, err
		}
		sessionID, parseErr := id.ParseSessionID("ses_" + strings.SplitN(attempt.ID.String(), "_", 2)[1])
		if parseErr != nil {
			return Transition{}, parseErr
		}
		callCtx, cancel := context.WithDeadline(ctx, deadline)
		record, err = environment.Execute(callCtx, sealed.Prepared, task.Input, engine.PreparedRunOptions{SessionID: sessionID, Purpose: run.PurposeOptimizationTrial, Provenance: run.ExecutionProvenance{ExperimentID: item.Experiment.ID.String(), TrialID: trial.ID.String(), CandidateID: trial.CandidateID.String(), EnvironmentReceiptID: attempt.Receipt.ID, HarnessID: sealed.HarnessID.String(), HarnessVersion: sealed.ID.String()}})
		cancel()
		if err != nil || record == nil {
			return Transition{}, errors.Join(ErrUnknownAttempt, err, h.stopAttempt(ctx, item, *attempt.Receipt))
		}
	}
	sealed, err := SealExecution(*record)
	if err != nil || sealed.Digest != record.Digest || !equalControllerData(record.Environment, *attempt.Receipt) || record.InputDigest != task.InputDigest {
		return Transition{}, errors.Join(ErrIncompleteEvidence, h.stopAttempt(ctx, item, *attempt.Receipt))
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	stop, err := h.config.Environments.Stop(cleanupCtx, *attempt.Receipt, Fence{Owner: item.Lease.Owner, Generation: item.Lease.Generation})
	if err != nil || stop.Validate() != nil || stop.State == StopUnknown || stop.AttemptID != attempt.ID || stop.EnvironmentReceiptID != attempt.Receipt.ID || stop.Fence != (Fence{Owner: item.Lease.Owner, Generation: item.Lease.Generation}) {
		return Transition{}, errors.Join(ErrUnknownAttempt, err)
	}
	update.State = TrialFailed
	switch record.State {
	case run.StateCompleted:
		update.State = TrialCompleted
	case run.StateCancelled:
		update.State = TrialCancelled
	}
	update.Execution, update.Stop = record, &stop
	change.TrialUpdates = []TrialUpdate{update}
	return change, nil
}
func (h *TrialPhaseHandler) closeEnvironment(ctx context.Context, e TrialEnvironment) error {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return e.Close(closeCtx)
}
func trialOperation(p Phase) OperationClass {
	switch p {
	case PhaseBaseline:
		return OperationBaseline
	case PhaseValidate:
		return OperationValidation
	default:
		return OperationProbe
	}
}
func (h *TrialPhaseHandler) settle(ctx context.Context, item WorkItem, trial Trial, record ExecutionRecord) error {
	attempt := trial.Attempts[len(trial.Attempts)-1]
	key := "trial/" + trial.ID.String() + "/" + attempt.ID.String()
	saved, err := h.config.Repository.GetReservation(ctx, item.Experiment.ID, key)
	if err != nil {
		return err
	}
	if saved.State == ReservationSettled {
		return nil
	}
	usage := record.Usage
	if usage.Receipt == "" {
		usage.Receipt = "capture/" + record.Digest
	}
	return h.meter.Settle(ctx, UsageSettlement{ExperimentID: item.Experiment.ID, AttemptID: attempt.ID, CallKey: key, Receipt: usage.Receipt, Usage: usage, Complete: usage.TokenQuality == UsageKnown && (item.Experiment.Spec.Budget.TokenOnly || usage.MoneyQuality == UsageKnown), Lease: item.Lease})
}

// ValidateTrialCapabilities refuses environments without required host guarantees.
func ValidateTrialCapabilities(c IsolationCapabilities, reproducible bool) error {
	if !c.Isolated || !c.Recoverable || !c.Stoppable || (!c.Resettable && !c.ReadOnly) || !c.FrozenRetrieval || !c.DeterministicApproval || reproducible && !c.AtomicSources {
		return ErrUnavailable
	}
	return nil
}

func (h *TrialPhaseHandler) stopAttempt(ctx context.Context, item WorkItem, receipt EnvironmentReceipt) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	stop, err := h.config.Environments.Stop(cleanup, receipt, Fence{Owner: item.Lease.Owner, Generation: item.Lease.Generation})
	if err != nil {
		return err
	}
	if stop.Validate() != nil || stop.State == StopUnknown || stop.AttemptID != receipt.AttemptID || stop.EnvironmentReceiptID != receipt.ID || stop.Fence != (Fence{Owner: item.Lease.Owner, Generation: item.Lease.Generation}) {
		return ErrUnknownAttempt
	}
	return nil
}
func (h *TrialPhaseHandler) checkArtifacts(ctx context.Context, trial Trial) error {
	return checkTrialArtifacts(ctx, h.config.Repository, trial)
}
func checkTrialArtifacts(ctx context.Context, repository Repository, trial Trial) error {
	var refs []ArtifactRef
	if trial.Execution != nil {
		refs = append(refs, trial.Execution.Artifacts...)
	}
	for _, attempt := range trial.Attempts {
		if attempt.Execution != nil {
			refs = append(refs, attempt.Execution.Artifacts...)
		}
	}
	for _, ref := range refs {
		if _, err := ref.BindingDigest(); err != nil || ref.ExperimentID != trial.ExperimentID {
			return ErrIncompleteEvidence
		}
		retained, err := repository.GetArtifact(ctx, ref)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if retained.Revision != 1 || retained.Deletion != nil || !retained.InvalidatedAt.IsZero() || !retained.DeletedAt.IsZero() || !equalControllerData(retained.Ref, ref) || retained.CreatedAt.IsZero() || now.Before(retained.CreatedAt) || !now.Before(retained.ExpiresAt) {
			return ErrIncompleteEvidence
		}
	}
	return nil
}
