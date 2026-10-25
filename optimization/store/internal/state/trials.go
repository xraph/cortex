package state

import (
	"context"
	"slices"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
)

// GetTrial returns the immutable trial identity and every persisted attempt.
func (s *Store) GetTrial(ctx context.Context, trialID id.ID) (optimization.Trial, error) {
	var empty optimization.Trial
	key, err := contextScope(ctx)
	if err != nil {
		return empty, err
	}
	unlock := s.lockOperation()
	defer unlock()
	if s.closed {
		return empty, optimization.ErrUnavailable
	}
	data, loadErr := s.loadResource(ctx, key, RecordKey{Kind: "trials", Key: trialID.String()})
	if loadErr != nil {
		return empty, loadErr
	}
	if data != nil {
		if trial, exists := data.Trials[trialID.String()]; exists {
			return clone(trial)
		}
	}
	return empty, optimization.ErrNotFound
}

type trialIdentity struct {
	version, task string
	repetition    int
}

func (tx *transaction) prepareTrials(experiment optimization.Experiment, change optimization.Transition, candidates candidateResources) (map[string]optimization.Trial, error) {
	result := make(map[string]optimization.Trial)
	if len(change.Trials)+len(change.TrialUpdates) == 0 {
		return result, nil
	}
	if experiment.State != optimization.StateRunning || change.NextState != optimization.StateRunning {
		return nil, optimization.ErrConflict
	}
	identities := make(map[trialIdentity]bool)
	discovery, validation := 0, 0
	for _, trial := range tx.data.Trials {
		if trial.ExperimentID == experiment.ID {
			identities[trialIdentity{trial.Version.Digest, trial.CaseDigest, trial.Repetition}] = true
			if trial.Split == optimization.SplitValidation {
				validation++
			} else {
				discovery++
			}
		}
	}
	for _, trial := range change.Trials {
		if _, err := optimization.ParseID(trial.ID.String(), optimization.PrefixTrial); err != nil {
			return nil, err
		}
		if _, exists := tx.data.Trials[trial.ID.String()]; exists {
			return nil, optimization.ErrConflict
		}
		if _, exists := result[trial.ID.String()]; exists {
			return nil, optimization.ErrConflict
		}
		if trial.ExperimentID != experiment.ID || trial.Revision != 0 || (trial.State != "" && trial.State != optimization.TrialQueued) || len(trial.Attempts) != 0 || trial.Execution != nil || trial.EvaluationID != "" || trial.Error != "" {
			return nil, optimization.ErrInvalidSpec
		}
		task, exists := trialTask(experiment, trial)
		if !exists || trial.Repetition < 0 || trial.Repetition >= experiment.Spec.Limits.Repetitions || (trial.Split != "" && trial.Split != task.Split) {
			return nil, optimization.ErrInvalidSpec
		}
		trial.Split = task.Split
		if err := tx.validateTrialPosition(experiment, trial, change.NextPhase, candidates); err != nil {
			return nil, err
		}
		identity := trialIdentity{trial.Version.Digest, trial.CaseDigest, trial.Repetition}
		if identities[identity] {
			return nil, optimization.ErrConflict
		}
		identities[identity] = true
		if trial.Split == optimization.SplitValidation {
			validation++
		} else {
			discovery++
		}
		if discovery > experiment.Plan.DiscoveryTrials || validation > experiment.Plan.ValidationTrials || discovery+validation > experiment.Spec.Limits.MaxTrials {
			return nil, optimization.ErrBudgetExceeded
		}
		trial.State = optimization.TrialQueued
		trial.Revision = 1
		trial.CreatedAt = tx.now
		trial.Attempts = []optimization.Attempt{}
		detached, err := clone(trial)
		if err != nil {
			return nil, err
		}
		result[trial.ID.String()] = detached
	}
	for _, update := range change.TrialUpdates {
		if _, exists := result[update.TrialID.String()]; exists {
			return nil, optimization.ErrConflict
		}
		trial, exists := tx.data.Trials[update.TrialID.String()]
		if !exists || trial.ExperimentID != experiment.ID {
			return nil, optimization.ErrNotFound
		}
		if trial.Revision != update.ExpectedRevision {
			return nil, optimization.ErrConflict
		}
		if err := tx.validateTrialPosition(experiment, trial, experiment.Phase, candidates); err != nil {
			return nil, err
		}
		detached, err := clone(trial)
		if err != nil {
			return nil, err
		}
		if err := tx.applyTrialUpdate(experiment, &detached, update, change.Lease, result); err != nil {
			return nil, err
		}
		detached.Revision++
		result[trial.ID.String()] = detached
	}
	return result, nil
}

func trialTask(experiment optimization.Experiment, trial optimization.Trial) (optimization.TaskCase, bool) {
	for _, task := range experiment.Spec.Tasks {
		if task.ID == trial.CaseID && task.Digest == trial.CaseDigest {
			return task, true
		}
	}
	return optimization.TaskCase{}, false
}

func (tx *transaction) validateTrialPosition(experiment optimization.Experiment, trial optimization.Trial, phase optimization.Phase, pending candidateResources) error {
	if trial.CandidateID.IsNil() {
		if trial.Version != experiment.Spec.Baseline {
			return optimization.ErrStaleVersion
		}
	} else {
		candidate, exists := pending.candidates[trial.CandidateID.String()]
		if !exists {
			candidate, exists = tx.data.Candidates[trial.CandidateID.String()]
		}
		if !exists || candidate.ExperimentID != experiment.ID || !candidate.Accepted {
			return optimization.ErrNotFound
		}
		if candidate.Version != trial.Version {
			return optimization.ErrStaleVersion
		}
	}
	switch phase {
	case optimization.PhaseBaseline:
		if trial.Split != optimization.SplitDiscovery || !trial.CandidateID.IsNil() {
			return optimization.ErrPolicyDenied
		}
	case optimization.PhaseProbe:
		if trial.Split != optimization.SplitDiscovery || trial.CandidateID.IsNil() {
			return optimization.ErrPolicyDenied
		}
	case optimization.PhaseValidate:
		if trial.Split != optimization.SplitValidation {
			return optimization.ErrPolicyDenied
		}
		return tx.validateTrialAllocation(experiment, trial, false)
	default:
		return optimization.ErrConflict
	}
	if !trial.AllocationID.IsNil() {
		return optimization.ErrInvalidSpec
	}
	return nil
}

func (tx *transaction) validateTrialAllocation(experiment optimization.Experiment, trial optimization.Trial, exposed bool) error {
	allocation, exists := tx.data.Allocations[trial.AllocationID.String()]
	if !exists {
		return optimization.ErrIncompleteEvidence
	}
	if rosterErr := tx.checkValidationRoster(allocation); rosterErr != nil {
		return rosterErr
	}
	if allocation.AgentID != experiment.Spec.AgentID || !allocation.ReleasedAt.IsZero() || !slices.Contains(allocation.CaseDigests, trial.CaseDigest) {
		return optimization.ErrPolicyDenied
	}
	for _, arm := range allocation.PairedRoster {
		if arm.ExperimentID != experiment.ID {
			continue
		}
		if arm.CaseDigests != nil && !slices.Contains(arm.CaseDigests, trial.CaseDigest) {
			return optimization.ErrPolicyDenied
		}
		if arm.Controller != experiment.Spec.Controller || arm.Baseline != experiment.Spec.Baseline || (trial.Version != arm.Baseline && trial.Version != arm.Finalist) {
			return optimization.ErrPolicyDenied
		}
		if exposed && allocation.ExposedAt.IsZero() {
			return optimization.ErrPolicyDenied
		}
		return nil
	}
	return optimization.ErrPolicyDenied
}
