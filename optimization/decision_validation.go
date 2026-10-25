package optimization

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// DecisionEvidence binds an immutable trial to its optional captured evaluation.
// Missing evaluations can support an inconclusive decision, never a recommendation.
type DecisionEvidence struct {
	Trial      Trial
	Evaluation *Evaluation
}

// CriterionFailureKey identifies the required criterion that failed in one trial.
// A byte-length prefix preserves case IDs and criterion IDs containing separators.
func CriterionFailureKey(caseID string, repetition int, criterionID string) string {
	return fmt.Sprintf("%d:%s/%d/%s", len(caseID), caseID, repetition, criterionID)
}

// ValidateDecisionEvidence checks stored identities and arithmetic before publication.
// The service must obtain the comparison from its trusted evaluator and separately
// check artifact availability, current authorization and runtime qualification.
// Bootstrap statistics remain the evaluator's responsibility.
func ValidateDecisionEvidence(experiment Experiment, decision Decision, evidence []DecisionEvidence) error {
	if experiment.Spec.Mode == ModeTaskLocal {
		return ValidateTaskDecision(experiment, decision, evidence)
	}
	if experiment.Spec.Improvement != nil || len(decision.Comparison.Plans) != 0 || decision.Comparison.PlanDigest != experiment.EvaluationPlan.Ref.Digest {
		return ErrIncompleteEvidence
	}
	if err := validateDecisionSummary(experiment, decision); err != nil {
		return err
	}
	comparison := decision.Comparison
	baseline := make(map[string]DecisionEvidence)
	candidate := make(map[string]DecisionEvidence)
	allIDs := make(map[string]bool)
	baselineIDs := make(map[string]bool)
	candidateIDs := make(map[string]bool)
	for _, item := range evidence {
		trial := item.Trial
		if trial.ExperimentID != experiment.ID || trial.AllocationID != decision.AllocationID || trial.Split != SplitValidation {
			return ErrIncompleteEvidence
		}
		key := fmt.Sprintf("%s/%d", trial.CaseID, trial.Repetition)
		var arm map[string]DecisionEvidence
		var ids map[string]bool
		switch trial.Version {
		case decision.Baseline:
			arm = baseline
			ids = baselineIDs
		case decision.Candidate:
			arm = candidate
			ids = candidateIDs
		default:
			return ErrIncompleteEvidence
		}
		if _, duplicate := arm[key]; duplicate {
			return ErrIncompleteEvidence
		}
		arm[key] = item
		if item.Evaluation != nil {
			if item.Evaluation.ID != trial.EvaluationID || item.Evaluation.ValidateCaptured(experiment, trial) != nil || allIDs[item.Evaluation.ID] {
				return ErrIncompleteEvidence
			}
			allIDs[item.Evaluation.ID] = true
			ids[item.Evaluation.ID] = true
		} else if trial.EvaluationID != "" || trial.Execution != nil {
			return ErrIncompleteEvidence
		}
	}
	if !sameEvidenceIDs(decision.EvaluationIDs, allIDs) || !sameEvidenceIDs(comparison.BaselineIDs, baselineIDs) || !sameEvidenceIDs(comparison.CandidateIDs, candidateIDs) {
		return ErrIncompleteEvidence
	}
	// Keeping the unmodified baseline has no independent candidate comparison.
	if decision.Candidate == decision.Baseline {
		if decision.Outcome != DecisionRetain && decision.Outcome != DecisionInconclusive || decision.EvidenceClass != EvidenceExploratory || comparison.Complete || len(comparison.Cases) != 0 || len(comparison.MissingReasons) == 0 || comparison.MeanDelta != 0 || comparison.LowerBound != 0 || comparison.UpperBound != 0 {
			return ErrIncompleteEvidence
		}
		return nil
	}
	stats, err := collectDecisionPairs(experiment, baseline, candidate)
	if err != nil {
		return err
	}
	return validateDecisionStatistics(experiment, decision, stats)
}

func validateDecisionSummary(experiment Experiment, decision Decision) error {
	if decision.TaskSelection != nil {
		return ErrIncompleteEvidence
	}
	if len(decision.Reasons) == 0 || slices.ContainsFunc(decision.Reasons, func(reason string) bool { return strings.TrimSpace(reason) == "" }) {
		return ErrIncompleteEvidence
	}
	switch decision.Outcome {
	case DecisionRecommend, DecisionRetain, DecisionInconclusive, DecisionReject:
	default:
		return ErrInvalidSpec
	}
	switch decision.EvidenceClass {
	case EvidenceValidated, EvidenceExploratory, EvidenceContrastive:
	default:
		return ErrInvalidSpec
	}
	if decision.Outcome == DecisionInconclusive && decision.EvidenceClass != EvidenceExploratory {
		return ErrIncompleteEvidence
	}
	comparison := decision.Comparison
	objective := experiment.Spec.Objective
	if comparison.Confidence != objective.Confidence || comparison.BootstrapSamples != objective.BootstrapSamples || comparison.BootstrapSeed != objective.BootstrapSeed {
		return ErrIncompleteEvidence
	}
	if !finite(comparison.MeanDelta) || !finite(comparison.LowerBound) || !finite(comparison.UpperBound) || comparison.MeanDelta < -1 || comparison.MeanDelta > 1 || comparison.LowerBound < -1 || comparison.UpperBound > 1 || comparison.LowerBound > comparison.UpperBound {
		return ErrIncompleteEvidence
	}
	for _, ratio := range []*float64{comparison.CostRatio, comparison.LatencyRatio} {
		if ratio != nil && (!finite(*ratio) || *ratio < 0) {
			return ErrIncompleteEvidence
		}
	}
	for _, reason := range comparison.MissingReasons {
		if strings.TrimSpace(reason) == "" {
			return ErrIncompleteEvidence
		}
	}
	return nil
}

func validateDecisionStatistics(experiment Experiment, decision Decision, stats decisionStats) error {
	comparison, objective := decision.Comparison, experiment.Spec.Objective
	if !sameEvidenceIDs(comparison.RequiredFailures, stats.failures) {
		return ErrIncompleteEvidence
	}
	if !stats.complete {
		if comparison.Complete || decision.Outcome != DecisionInconclusive || decision.EvidenceClass != EvidenceExploratory || len(comparison.MissingReasons) == 0 || len(comparison.Cases) != 0 || comparison.MeanDelta != 0 || comparison.LowerBound != 0 || comparison.UpperBound != 0 {
			return ErrIncompleteEvidence
		}
		return nil
	}
	if len(comparison.Cases) != len(stats.cases) {
		return ErrIncompleteEvidence
	}
	seen := make(map[string]bool)
	var mean float64
	for _, got := range comparison.Cases {
		expected, ok := stats.cases[got.CaseID]
		if !ok || seen[got.CaseID] || got.CaseDigest != expected.CaseDigest || got.Repetitions != expected.Repetitions || !near(got.Delta, expected.Delta) {
			return ErrIncompleteEvidence
		}
		seen[got.CaseID] = true
		for criterion, delta := range got.Criteria {
			if value, found := expected.Criteria[criterion]; !found || !near(delta, value) {
				return ErrIncompleteEvidence
			}
		}
		for _, criterion := range objective.ProtectedCriteria {
			if value, found := got.Criteria[criterion]; !found || !near(value, expected.Criteria[criterion]) {
				return ErrIncompleteEvidence
			}
		}
		mean += expected.Delta
	}
	mean /= float64(len(stats.cases))
	if !near(comparison.MeanDelta, mean) {
		return ErrIncompleteEvidence
	}
	for criterion, regression := range comparison.ProtectedRegressions {
		if !slices.Contains(objective.ProtectedCriteria, criterion) || !near(regression, stats.regressions[criterion]) {
			return ErrIncompleteEvidence
		}
	}
	protectedPass := true
	for _, criterion := range objective.ProtectedCriteria {
		regression, exists := comparison.ProtectedRegressions[criterion]
		if !exists || !near(regression, stats.regressions[criterion]) {
			return ErrIncompleteEvidence
		}
		protectedPass = protectedPass && regression <= objective.MaxProtectedRegression
	}
	if !validRatio(comparison.CostRatio, stats.costKnown, stats.baselineCost, stats.candidateCost) || !validRatio(comparison.LatencyRatio, true, stats.baselineLatency, stats.candidateLatency) {
		return ErrIncompleteEvidence
	}
	complete := len(stats.cases) >= objective.MinValidationCases
	if comparison.Complete && (!complete || len(comparison.MissingReasons) > 0) || !comparison.Complete && len(comparison.MissingReasons) == 0 {
		return ErrIncompleteEvidence
	}
	if !comparison.Complete {
		if decision.Outcome != DecisionInconclusive || decision.EvidenceClass != EvidenceExploratory {
			return ErrIncompleteEvidence
		}
		return nil
	}
	if decision.EvidenceClass == EvidenceExploratory && decision.Outcome != DecisionInconclusive {
		return ErrIncompleteEvidence
	}
	if decision.Outcome == DecisionRecommend {
		if !stats.isolated || objective.RequireReproducibility && !stats.reproducible || experiment.Spec.Mode == ModeTaskLocal || decision.EvidenceClass != EvidenceValidated || len(stats.failures) > 0 || !protectedPass || mean < objective.MinImprovement || comparison.LowerBound <= 0 {
			return ErrIncompleteEvidence
		}
		if !ratioPass(objective.MaxCostRatio, comparison.CostRatio) || !ratioPass(objective.MaxLatencyRatio, comparison.LatencyRatio) {
			return ErrIncompleteEvidence
		}
	}
	return nil
}

type decisionStats struct {
	cases                                                          map[string]CaseDelta
	failures                                                       map[string]bool
	regressions                                                    map[string]float64
	complete, costKnown, isolated, reproducible                    bool
	baselineCost, candidateCost, baselineLatency, candidateLatency float64
}

func collectDecisionPairs(experiment Experiment, baseline, candidate map[string]DecisionEvidence) (decisionStats, error) {
	result := decisionStats{cases: make(map[string]CaseDelta), failures: make(map[string]bool), regressions: make(map[string]float64), complete: true, costKnown: true, isolated: true, reproducible: true}
	count := 0
	for _, task := range experiment.Spec.Tasks {
		if task.Split != SplitValidation {
			continue
		}
		count++
		value := CaseDelta{CaseID: task.ID, CaseDigest: task.Digest, Repetitions: experiment.Spec.Limits.Repetitions, Criteria: make(map[string]float64)}
		for rep := range value.Repetitions {
			key := fmt.Sprintf("%s/%d", task.ID, rep)
			b, bok := baseline[key]
			c, cok := candidate[key]
			if !bok || !cok || b.Trial.CaseDigest != task.Digest || c.Trial.CaseDigest != task.Digest {
				return result, ErrIncompleteEvidence
			}
			for _, item := range []DecisionEvidence{b, c} {
				switch item.Trial.State {
				case TrialCompleted, TrialFailed, TrialCancelled, TrialBlocked:
				default:
					return result, ErrIncompleteEvidence
				}
				if item.Evaluation == nil && strings.TrimSpace(item.Trial.Error) == "" {
					return result, ErrIncompleteEvidence
				}
			}
			if c.Evaluation != nil {
				for _, criterion := range c.Evaluation.Criteria {
					if criterion.Required && !criterion.Passed {
						result.failures[CriterionFailureKey(task.ID, rep, criterion.ID)] = true
					}
				}
			}
			if b.Evaluation == nil || c.Evaluation == nil || !b.Evaluation.Complete || !c.Evaluation.Complete {
				result.complete = false
				continue
			}
			value.Delta += *c.Evaluation.Score - *b.Evaluation.Score
			bcriteria := make(map[string]float64)
			for _, criterion := range b.Evaluation.Criteria {
				bcriteria[criterion.ID] = *criterion.Score
			}
			for _, criterion := range c.Evaluation.Criteria {
				value.Criteria[criterion.ID] += *criterion.Score - bcriteria[criterion.ID]
			}
			be, ce := b.Trial.Execution, c.Trial.Execution
			for _, execution := range []*ExecutionRecord{be, ce} {
				capabilities := execution.Environment.Capabilities
				result.isolated = result.isolated && capabilities.Isolated
				result.reproducible = result.reproducible && capabilities.AtomicSources && capabilities.FrozenRetrieval
			}
			result.baselineLatency += float64(be.Duration)
			result.candidateLatency += float64(ce.Duration)
			if be.Usage.MoneyQuality != UsageKnown || ce.Usage.MoneyQuality != UsageKnown || be.Usage.PricingRevision != experiment.Spec.Budget.PricingRevision || ce.Usage.PricingRevision != experiment.Spec.Budget.PricingRevision {
				result.costKnown = false
			}
			result.baselineCost += float64(be.Usage.USDMicros)
			result.candidateCost += float64(ce.Usage.USDMicros)
		}
		value.Delta /= float64(value.Repetitions)
		for criterion, delta := range value.Criteria {
			value.Criteria[criterion] = delta / float64(value.Repetitions)
		}
		result.cases[task.ID] = value
	}
	if count == 0 || len(baseline) != count*experiment.Spec.Limits.Repetitions || len(candidate) != len(baseline) {
		return result, ErrIncompleteEvidence
	}
	for _, criterion := range experiment.Spec.Objective.ProtectedCriteria {
		var delta float64
		for _, value := range result.cases {
			delta += value.Criteria[criterion]
		}
		result.regressions[criterion] = math.Max(0, -delta/float64(count))
	}
	return result, nil
}

func sameEvidenceIDs(actual []string, expected map[string]bool) bool {
	if len(actual) != len(expected) {
		return false
	}
	seen := make(map[string]bool, len(actual))
	for _, value := range actual {
		if !expected[value] || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}
func near(a, b float64) bool { return finite(a) && finite(b) && math.Abs(a-b) <= 1e-12 }
func validRatio(value *float64, known bool, baseline, candidate float64) bool {
	return value == nil || known && baseline > 0 && near(*value, candidate/baseline)
}
func ratioPass(limit, value *float64) bool { return limit == nil || value != nil && *value <= *limit }
