package optimization

import (
	"math"
	"slices"
	"strings"

	"github.com/xraph/cortex/run"
)

// Validate binds public plan metadata to the frozen experiment inputs.
// Authenticating the external plan and private rubric remains the adapter's job.
func (plan EvaluationPlan) Validate(spec ExperimentSpec) error {
	if plan.Ref != spec.Evaluation || plan.Ref.Validate() != nil || plan.FrozenAt.IsZero() || len(plan.CaseDigests) != len(spec.Tasks) || len(plan.Scorers) == 0 || len(plan.Criteria) == 0 {
		return ErrInvalidSpec
	}
	for _, task := range spec.Tasks {
		if plan.CaseDigests[task.ID] != task.Digest {
			return ErrInvalidSpec
		}
	}
	scorers := make(map[string]bool, len(plan.Scorers))
	for _, scorer := range plan.Scorers {
		if strings.TrimSpace(scorer.Name) == "" || scorers[scorer.Name] || strings.TrimSpace(scorer.Revision) == "" && scorer.Digest == "" {
			return ErrInvalidSpec
		}
		if scorer.Digest != "" && !validDigest(scorer.Digest) {
			return ErrInvalidSpec
		}
		scorers[scorer.Name] = true
	}
	criteria := make(map[string]bool, len(plan.Criteria))
	required := make(map[string]bool, len(plan.Criteria))
	for _, criterion := range plan.Criteria {
		if strings.TrimSpace(criterion.ID) == "" || criteria[criterion.ID] {
			return ErrInvalidSpec
		}
		criteria[criterion.ID] = true
		required[criterion.ID] = criterion.Required
	}
	for _, name := range spec.Objective.RequiredCriteria {
		if !required[name] {
			return ErrInvalidSpec
		}
	}
	for _, name := range spec.Objective.ProtectedCriteria {
		if !criteria[name] {
			return ErrInvalidSpec
		}
	}
	return nil
}

// ValidateCaptured checks scoring identity and completeness without executing a target.
func (evaluation Evaluation) ValidateCaptured(experiment Experiment, trial Trial) error {
	if strings.TrimSpace(evaluation.ID) == "" || strings.TrimSpace(evaluation.ReportID) == "" ||
		evaluation.ExperimentID != experiment.ID || evaluation.TrialID != trial.ID ||
		evaluation.CaseID != trial.CaseID || evaluation.CaseDigest != trial.CaseDigest ||
		evaluation.Repetition != trial.Repetition || trial.Execution == nil ||
		evaluation.PlanDigest != experiment.EvaluationPlan.Ref.Digest || evaluation.ExecutionDigest != trial.Execution.Digest {
		return ErrIncompleteEvidence
	}
	if evaluation.StartedAt.IsZero() || evaluation.CompletedAt.IsZero() || evaluation.CompletedAt.Before(evaluation.StartedAt) {
		return ErrIncompleteEvidence
	}
	if err := evaluation.JudgeUsage.Validate(); err != nil {
		return err
	}
	if !sameScorers(evaluation.Scorers, experiment.EvaluationPlan.Scorers) {
		return ErrIncompleteEvidence
	}
	expected := make(map[string]bool, len(experiment.EvaluationPlan.Criteria))
	for _, criterion := range experiment.EvaluationPlan.Criteria {
		expected[criterion.ID] = criterion.Required
	}
	if len(evaluation.Criteria) != len(expected) {
		return ErrIncompleteEvidence
	}
	seen := make(map[string]bool, len(evaluation.Criteria))
	incomplete := len(evaluation.Errors) > 0 || trial.State != TrialCompleted || trial.Execution.State != run.StateCompleted
	for _, message := range evaluation.Errors {
		if strings.TrimSpace(message) == "" {
			return ErrInvalidSpec
		}
	}
	for _, criterion := range evaluation.Criteria {
		required, exists := expected[criterion.ID]
		if !exists || seen[criterion.ID] || required != criterion.Required {
			return ErrIncompleteEvidence
		}
		seen[criterion.ID] = true
		if criterion.Score != nil {
			score := *criterion.Score
			if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 1 || criterion.Error != "" {
				return ErrIncompleteEvidence
			}
		} else {
			if strings.TrimSpace(criterion.Error) == "" || criterion.Passed {
				return ErrIncompleteEvidence
			}
			incomplete = true
		}
	}
	if evaluation.Complete && incomplete {
		return ErrIncompleteEvidence
	}
	if !evaluation.Complete && !incomplete {
		return ErrIncompleteEvidence
	}
	return nil
}

func sameScorers(left, right []run.Revision) bool {
	if len(left) != len(right) {
		return false
	}
	actual := make(map[string]run.Revision, len(left))
	for _, scorer := range left {
		if _, exists := actual[scorer.Name]; exists {
			return false
		}
		actual[scorer.Name] = scorer
	}
	return !slices.ContainsFunc(right, func(scorer run.Revision) bool { return actual[scorer.Name] != scorer })
}
