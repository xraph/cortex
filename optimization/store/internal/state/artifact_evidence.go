package state

import (
	"slices"
	"time"

	"github.com/xraph/cortex/optimization"
)

func decisionTrials(data *scopeData, decision optimization.Decision) []optimization.Trial {
	var trials []optimization.Trial
	experiment := data.Experiments[decision.ExperimentID.String()]
	for _, trial := range data.Trials {
		if trial.ExperimentID == decision.ExperimentID && (experiment.Spec.Mode == optimization.ModeTaskLocal || trial.Split == optimization.SplitValidation && trial.AllocationID == decision.AllocationID) {
			trials = append(trials, trial)
		} else if experiment.Spec.Improvement != nil && decision.SelectionKind == string(optimization.HarnessImprovement) && trial.Split == optimization.SplitValidation && trial.AllocationID == decision.AllocationID {
			allocation, exists := data.Allocations[decision.AllocationID.String()]
			if exists && allocation.ExperimentID == experiment.ID && slices.ContainsFunc(allocation.PairedRoster, func(arm optimization.ValidationArm) bool {
				return arm.ExperimentID == trial.ExperimentID && (trial.Version == arm.Baseline || trial.Version == arm.Finalist)
			}) {
				trials = append(trials, trial)
			}
		}
	}
	return trials
}

func checkTrialArtifacts(data *scopeData, trial optimization.Trial, now time.Time) error {
	for _, ref := range optimization.TrialArtifactRefs(trial) {
		key, err := ref.BindingDigest()
		if err != nil || ref.ExperimentID != trial.ExperimentID {
			return optimization.ErrIncompleteEvidence
		}
		value, exists := data.Artifacts[key]
		if !exists || value.Revision != 1 || value.Deletion != nil || !value.InvalidatedAt.IsZero() || !value.DeletedAt.IsZero() || !same(value.Ref, ref) || value.CreatedAt.IsZero() || now.Before(value.CreatedAt) || !now.Before(value.ExpiresAt) {
			return optimization.ErrIncompleteEvidence
		}
	}
	return nil
}

func checkDecisionArtifacts(data *scopeData, decision optimization.Decision, now time.Time) error {
	trials := decisionTrials(data, decision)
	for _, evaluationID := range decision.EvaluationIDs {
		evaluation, exists := data.Evaluations[evaluationID]
		if !exists {
			return optimization.ErrIncompleteEvidence
		}
		if !slices.ContainsFunc(trials, func(trial optimization.Trial) bool {
			return trial.ID == evaluation.TrialID && trial.ExperimentID == evaluation.ExperimentID && trial.EvaluationID == evaluationID
		}) {
			return optimization.ErrIncompleteEvidence
		}
	}
	for _, trial := range trials {
		if err := checkTrialArtifacts(data, trial, now); err != nil {
			return err
		}
	}
	return nil
}

func trialUsesArtifact(trial optimization.Trial, binding string) bool {
	for _, ref := range optimization.TrialArtifactRefs(trial) {
		key, err := ref.BindingDigest()
		if err == nil && key == binding {
			return true
		}
	}
	return false
}

func (tx *transaction) revokeArtifactLessons(binding string, request optimization.ArtifactDeletion) error {
	keys := make([]string, 0, len(tx.data.Lessons))
	for key := range tx.data.Lessons {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	revoked := make(map[string]bool)
	for {
		changed := false
		for _, key := range keys {
			lesson := tx.data.Lessons[key]
			if !lesson.RevokedAt.IsZero() {
				continue
			}
			affected := false
			for _, trialID := range lesson.TrialIDs {
				if trialUsesArtifact(tx.data.Trials[trialID.String()], binding) {
					affected = true
				}
			}
			for _, decisionID := range lesson.DecisionIDs {
				decision, exists := tx.data.Decisions[decisionID.String()]
				if !exists {
					return optimization.ErrIncompleteEvidence
				}
				for _, trial := range decisionTrials(tx.data, decision) {
					if trialUsesArtifact(trial, binding) {
						affected = true
					}
				}
				for _, sourceLesson := range tx.data.Experiments[decision.ExperimentID.String()].LessonIDs {
					if revoked[sourceLesson.String()] {
						affected = true
					}
				}
			}
			if !affected {
				continue
			}
			if err := tx.RevokeLesson(optimization.LessonRevocation{LessonID: lesson.ID, ExpectedRevision: lesson.Revision, Actor: request.Actor, Reason: "artifact evidence withdrawn: " + request.RequestID}); err != nil {
				return err
			}
			revoked[key], changed = true, true
		}
		if !changed {
			return nil
		}
	}
}
