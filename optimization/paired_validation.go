package optimization

import (
	"context"
	"slices"
	"strings"
)

// TrialEvidenceReady reports whether a terminal trial has the evidence needed
// for selection. Failed trials remain outcomes; missing evaluations do not.
func TrialEvidenceReady(trial Trial) bool {
	switch trial.State {
	case TrialCompleted:
		return trial.Execution != nil && trial.EvaluationID != ""
	case TrialFailed, TrialCancelled, TrialBlocked:
		if trial.Execution != nil {
			return trial.EvaluationID != ""
		}
		return strings.TrimSpace(trial.Error) != ""
	default:
		return false
	}
}

func assignedValidationArm(experiment Experiment, allocation ValidationAllocation) (ValidationArm, error) {
	if !allocation.ReleasedAt.IsZero() || allocation.AgentID != experiment.Spec.AgentID || allocation.Scope.Canonical() != experiment.Spec.Scope.Canonical() {
		return ValidationArm{}, ErrIncompleteEvidence
	}
	if experiment.Spec.Parent == nil {
		if allocation.ExperimentID != experiment.ID || len(allocation.PairedRoster) != 1 {
			return ValidationArm{}, ErrPolicyDenied
		}
	} else if allocation.ExperimentID != experiment.Spec.Parent.ExperimentID || len(allocation.PairedRoster) < 2 {
		return ValidationArm{}, ErrPolicyDenied
	}
	var found *ValidationArm
	for _, arm := range allocation.PairedRoster {
		if arm.ExperimentID != experiment.ID {
			continue
		}
		if found != nil || arm.Controller != experiment.Spec.Controller || arm.Baseline != experiment.Spec.Baseline {
			return ValidationArm{}, ErrPolicyDenied
		}
		found = &arm
	}
	if found == nil {
		return ValidationArm{}, ErrIncompleteEvidence
	}
	if experiment.Spec.Parent != nil {
		cases := []string{}
		for _, task := range experiment.Spec.Tasks {
			if task.Split == SplitValidation {
				cases = append(cases, task.Digest)
			}
		}
		slices.Sort(cases)
		if len(cases) == 0 || !slices.Equal(cases, found.CaseDigests) {
			return ValidationArm{}, ErrPolicyDenied
		}
	}
	return *found, nil
}

func (h *TrialPhaseHandler) pairedValidationReady(ctx context.Context, experiment Experiment, allocation ValidationAllocation) error {
	if experiment.Spec.Parent == nil {
		return nil
	}
	for _, arm := range allocation.PairedRoster {
		child, err := h.config.Repository.GetExperiment(ctx, arm.ExperimentID)
		if err != nil {
			return err
		}
		if child.Spec.Parent == nil || child.Spec.Parent.ExperimentID != experiment.Spec.Parent.ExperimentID {
			return ErrPolicyDenied
		}
		if _, bindingErr := assignedValidationArm(child, allocation); bindingErr != nil {
			return bindingErr
		}
		trials, err := h.listTrials(ctx, child.ID)
		if err != nil {
			return err
		}
		if !validationArmReady(child, arm, allocation.ID, trials) {
			return ErrConflict
		}
	}
	return nil
}
