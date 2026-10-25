package optimization

import (
	"slices"
	"strings"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

// TaskSelection identifies an already completed trial. Its score is contrastive
// evidence for this task, not a confidence estimate or an activation approval.
type TaskSelection struct {
	TrialID id.ID   `json:"trial_id"`
	Score   float64 `json:"score"`
}

// BuildTaskDecision deterministically selects from the complete stored roster.
// The caller supplies decision identity and the frozen baseline policy digest.
func BuildTaskDecision(experiment Experiment, evidence []DecisionEvidence) (Decision, error) {
	d := Decision{ExperimentID: experiment.ID, SpecDigest: experiment.SpecDigest, Baseline: experiment.Spec.Baseline, Candidate: experiment.Spec.Baseline, SelectionKind: string(HarnessExecution), Outcome: DecisionInconclusive, EvidenceClass: EvidenceExploratory, Reasons: []string{"No completed task trial passed selection gates."}}
	if experiment.Spec.Mode != ModeTaskLocal || len(experiment.Spec.Tasks) != 1 || experiment.EvaluationPlan.Validate(experiment.Spec) != nil {
		return d, ErrInvalidSpec
	}
	roster := slices.Clone(evidence)
	slices.SortFunc(roster, func(a, b DecisionEvidence) int { return strings.Compare(a.Trial.ID.String(), b.Trial.ID.String()) })
	var eligible []DecisionEvidence
	var baselineReference *DecisionEvidence
	observations := 0
	seen, evaluations := map[id.ID]bool{}, map[string]bool{}
	task := experiment.Spec.Tasks[0]
	for _, value := range roster {
		trial := value.Trial
		if seen[trial.ID] || trial.ExperimentID != experiment.ID || trial.Split != SplitDiscovery || !trial.AllocationID.IsNil() || trial.CaseID != task.ID || trial.CaseDigest != task.Digest || trial.Repetition < 0 || trial.Repetition >= experiment.Spec.Limits.Repetitions {
			return d, ErrIncompleteEvidence
		}
		seen[trial.ID] = true
		if value.Evaluation == nil {
			if trial.Execution != nil || trial.EvaluationID != "" || trial.Error == "" || trial.State != TrialBlocked && trial.State != TrialFailed && trial.State != TrialCancelled {
				return d, ErrIncompleteEvidence
			}
			continue
		}
		evaluation := value.Evaluation
		if evaluation.ID != trial.EvaluationID || evaluations[evaluation.ID] || evaluation.ValidateCaptured(experiment, trial) != nil {
			return d, ErrIncompleteEvidence
		}
		evaluations[evaluation.ID] = true
		d.EvaluationIDs = append(d.EvaluationIDs, evaluation.ID)
		if !evaluation.Complete || evaluation.Score == nil || trial.State != TrialCompleted || trial.Execution.State != run.StateCompleted {
			continue
		}
		if validateTrialEnvironment(experiment.Spec, trial.Execution.Environment.Capabilities) != nil {
			continue
		}
		observations++
		if trial.Version == d.Baseline && (baselineReference == nil || *evaluation.Score > *baselineReference.Evaluation.Score) {
			baselineReference = &value
		}
		passed := true
		for _, criterion := range evaluation.Criteria {
			passed = passed && (!criterion.Required || criterion.Passed && criterion.Error == "")
		}
		if passed {
			eligible = append(eligible, value)
		}
	}
	slices.Sort(d.EvaluationIDs)
	var baseline, chosen *DecisionEvidence
	for i := range eligible {
		value := &eligible[i]
		if value.Trial.Version == d.Baseline && (baseline == nil || *value.Evaluation.Score > *baseline.Evaluation.Score) {
			baseline = value
		}
	}
	chosen = baseline
	for i := range eligible {
		value := &eligible[i]
		if value.Trial.Version == d.Baseline || !taskCandidatePasses(experiment.Spec, baselineReference, *value) {
			continue
		}
		if chosen == nil || *value.Evaluation.Score > *chosen.Evaluation.Score {
			chosen = value
		}
	}
	if chosen == nil {
		return d, nil
	}
	d.Candidate = chosen.Trial.Version
	d.TaskSelection = &TaskSelection{TrialID: chosen.Trial.ID, Score: *chosen.Evaluation.Score}
	d.Outcome = DecisionSelect
	if d.Candidate == d.Baseline {
		d.Outcome = DecisionRetain
	}
	if observations > 1 {
		d.EvidenceClass = EvidenceContrastive
	}
	d.Reasons = []string{"Selected the completed task trial that passed the frozen scoring and resource gates."}
	return d, nil
}

func taskCandidatePasses(spec ExperimentSpec, baseline *DecisionEvidence, candidate DecisionEvidence) bool {
	o := spec.Objective
	if baseline == nil {
		return len(o.ProtectedCriteria) == 0 && o.MaxCostRatio == nil && o.MaxLatencyRatio == nil
	}
	if *candidate.Evaluation.Score-*baseline.Evaluation.Score < o.MinImprovement {
		return false
	}
	for _, name := range o.ProtectedCriteria {
		var before, after *float64
		for _, value := range baseline.Evaluation.Criteria {
			if value.ID == name {
				before = value.Score
			}
		}
		for _, value := range candidate.Evaluation.Criteria {
			if value.ID == name {
				after = value.Score
			}
		}
		if before == nil || after == nil || *before-*after > o.MaxProtectedRegression {
			return false
		}
	}
	if o.MaxCostRatio != nil {
		a, b := baseline.Trial.Execution.Usage, candidate.Trial.Execution.Usage
		before, beforeKnown := a.KnownCost()
		after, afterKnown := b.KnownCost()
		if !beforeKnown || !afterKnown || before <= 0 || a.PricingRevision != spec.Budget.PricingRevision || b.PricingRevision != spec.Budget.PricingRevision || float64(after) > float64(before)**o.MaxCostRatio {
			return false
		}
	}
	return o.MaxLatencyRatio == nil || baseline.Trial.Execution.Duration > 0 && float64(candidate.Trial.Execution.Duration) <= float64(baseline.Trial.Execution.Duration)**o.MaxLatencyRatio
}

// ValidateTaskDecision rejects fabricated selection, scores or missing outcomes.
func ValidateTaskDecision(experiment Experiment, decision Decision, evidence []DecisionEvidence) error {
	expected, err := BuildTaskDecision(experiment, evidence)
	if err != nil {
		return err
	}
	if decision.Baseline != expected.Baseline || decision.Candidate != expected.Candidate || decision.Outcome != expected.Outcome || decision.EvidenceClass != expected.EvidenceClass || !equalControllerData(decision.TaskSelection, expected.TaskSelection) || !equalControllerData(decision.EvaluationIDs, expected.EvaluationIDs) || !decision.AllocationID.IsNil() || !equalControllerData(decision.Comparison, Comparison{}) || !equalControllerData(decision.Reasons, expected.Reasons) {
		return ErrIncompleteEvidence
	}
	return nil
}
