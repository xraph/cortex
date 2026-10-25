package state

import (
	"slices"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
)

func (tx *transaction) validatePhaseEvidence(experiment optimization.Experiment, change optimization.Transition, candidates candidateResources, trials map[string]optimization.Trial) error {
	if experiment.State != optimization.StateRunning || change.NextState != optimization.StateRunning || experiment.Phase == change.NextPhase {
		return nil
	}
	switch experiment.Phase {
	case optimization.PhaseBaseline:
		if experiment.Spec.Improvement != nil {
			for _, child := range experiment.Spec.Improvement.Children {
				current, exists := tx.data.Experiments[child.ExperimentID.String()]
				if !exists || current.State != optimization.StateRunning || current.Phase != optimization.PhaseValidate {
					return optimization.ErrIncompleteEvidence
				}
				batch, recorded := tx.data.DiscoveryLessons[current.ID.String()]
				if !recorded {
					return optimization.ErrIncompleteEvidence
				}
				if err := checkLessonBindings(tx.data, current, tx.now); err != nil {
					return err
				}
				if err := checkDiscoveryEvidence(tx.data, current, batch.Lessons, tx.now); err != nil {
					return err
				}
			}
			return nil
		}
		return tx.requireVersionTrials(experiment, experiment.Spec.Baseline, optimization.SplitDiscovery, id.ID{}, trials)
	case optimization.PhasePropose, optimization.PhaseCompose:
		if len(candidates.candidates) == 0 {
			return optimization.ErrIncompleteEvidence
		}
	case optimization.PhaseProbe:
		count := 0
		for _, candidate := range tx.data.Candidates {
			if candidate.ExperimentID != experiment.ID || candidate.Round != experiment.Round {
				continue
			}
			count++
			if candidate.Accepted {
				if err := tx.requireVersionTrials(experiment, candidate.Version, optimization.SplitDiscovery, id.ID{}, trials); err != nil {
					return err
				}
			}
		}
		if count == 0 {
			return optimization.ErrIncompleteEvidence
		}
	case optimization.PhaseValidate:
		for _, allocation := range tx.data.Allocations {
			if allocation.ExposedAt.IsZero() || !allocation.ReleasedAt.IsZero() {
				continue
			}
			member := allocation.ExperimentID == experiment.ID || slices.ContainsFunc(allocation.PairedRoster, func(arm optimization.ValidationArm) bool { return arm.ExperimentID == experiment.ID })
			if !member {
				continue
			}
			if rosterErr := tx.checkValidationRoster(allocation); rosterErr != nil {
				return rosterErr
			}
			for _, arm := range allocation.PairedRoster {
				child, exists := tx.data.Experiments[arm.ExperimentID.String()]
				if !exists || experiment.Spec.Improvement != nil && child.State != optimization.StateRunning {
					return optimization.ErrIncompleteEvidence
				}
				if err := tx.requireVersionTrials(child, arm.Baseline, optimization.SplitValidation, allocation.ID, trials); err != nil {
					return err
				}
				if err := tx.requireVersionTrials(child, arm.Finalist, optimization.SplitValidation, allocation.ID, trials); err != nil {
					return err
				}
			}
			return nil
		}
		return optimization.ErrIncompleteEvidence
	}
	return nil
}

func (tx *transaction) requireVersionTrials(experiment optimization.Experiment, version optimization.VersionRef, split optimization.Split, allocation id.ID, pending map[string]optimization.Trial) error {
	type caseRepetition struct {
		digest     string
		repetition int
	}
	ready := make(map[caseRepetition]bool)
	tx.anyTrial(pending, func(trial optimization.Trial) bool {
		if trial.ExperimentID == experiment.ID && trial.Version == version && trial.Split == split && trial.AllocationID == allocation && optimization.TrialEvidenceReady(trial) {
			ready[caseRepetition{trial.CaseDigest, trial.Repetition}] = true
		}
		return false
	})
	count := 0
	for _, task := range experiment.Spec.Tasks {
		if task.Split != split {
			continue
		}
		count++
		for repetition := range experiment.Spec.Limits.Repetitions {
			if !ready[caseRepetition{task.Digest, repetition}] {
				return optimization.ErrIncompleteEvidence
			}
		}
	}
	if count == 0 {
		return optimization.ErrIncompleteEvidence
	}
	return nil
}
