package optimization

import (
	"context"
	"fmt"
	"slices"

	"github.com/xraph/cortex/id"
)

// ControllerEvaluator compares the selected executions of frozen controller arms.
// The adapter must reload authoritative evidence before computing statistics.
type ControllerEvaluator interface {
	CompareControllers(context.Context, ControllerComparisonRequest) (Comparison, error)
}

// ControllerComparisonRequest identifies a parent allocation and its scored groups.
type ControllerComparisonRequest struct {
	ExperimentID id.ID                       `json:"experiment_id"`
	AllocationID id.ID                       `json:"allocation_id"`
	SpecDigest   string                      `json:"spec_digest"`
	Objective    Objective                   `json:"objective"`
	Groups       []ControllerComparisonGroup `json:"groups"`
}

// ControllerComparisonGroup preserves one frozen plan and the original job IDs.
type ControllerComparisonGroup struct {
	Plan         EvaluationPlan `json:"plan"`
	BaselineIDs  []string       `json:"baseline_ids"`
	CandidateIDs []string       `json:"candidate_ids"`
}

// ControllerEvidence retains a child's actual identity and selected validation trials.
type ControllerEvidence struct {
	Experiment Experiment         `json:"experiment"`
	Trials     []DecisionEvidence `json:"trials"`
}

// BuildControllerComparison validates the frozen roster without rewriting child
// experiment IDs, execution versions or evaluation plans to resemble the parent.
func BuildControllerComparison(parent Experiment, allocation ValidationAllocation, children []ControllerEvidence) (ControllerComparisonRequest, error) {
	request, _, _, err := controllerComparisonEvidence(parent, allocation, children)
	return request, err
}

// ReadControllerEvidence loads the selected child trials in the caller's scope.
// Callers must check current host authority for the parent and every child.
func ReadControllerEvidence(ctx context.Context, repo Repository, parent Experiment, allocation ValidationAllocation) ([]ControllerEvidence, error) {
	if repo == nil || parent.Spec.Improvement == nil {
		return nil, ErrInvalidSpec
	}
	children := make([]ControllerEvidence, 0, len(allocation.PairedRoster))
	for _, arm := range allocation.PairedRoster {
		child, err := repo.GetExperiment(ctx, arm.ExperimentID)
		if err != nil {
			return nil, err
		}
		trials, err := listExperimentTrials(ctx, repo, child.ID)
		if err != nil {
			return nil, err
		}
		value := ControllerEvidence{Experiment: child}
		for _, trial := range trials {
			if trial.Split != SplitValidation || trial.AllocationID != allocation.ID || trial.Version != arm.Finalist {
				continue
			}
			item := DecisionEvidence{Trial: trial}
			if trial.EvaluationID != "" {
				evaluation, getErr := repo.GetEvaluation(ctx, trial.EvaluationID)
				if getErr != nil {
					return nil, getErr
				}
				item.Evaluation = &evaluation
			}
			value.Trials = append(value.Trials, item)
		}
		children = append(children, value)
	}
	if _, err := BuildControllerComparison(parent, allocation, children); err != nil {
		return nil, err
	}
	return children, nil
}

func controllerComparisonEvidence(parent Experiment, allocation ValidationAllocation, children []ControllerEvidence) (request ControllerComparisonRequest, baseline, candidate map[string]DecisionEvidence, err error) {
	plan := parent.Spec.Improvement
	if plan == nil || ValidateSpec(parent.Spec) != nil || parent.EvaluationPlan.Validate(parent.Spec) != nil || len(children) != len(plan.Children) || len(allocation.PairedRoster) != len(children) || allocation.ExperimentID != parent.ID || allocation.Scope.Canonical() != parent.Spec.Scope.Canonical() || allocation.AgentID != parent.Spec.AgentID || allocation.ExposedAt.IsZero() || !allocation.ReleasedAt.IsZero() {
		return request, nil, nil, ErrIncompleteEvidence
	}
	request = ControllerComparisonRequest{ExperimentID: parent.ID, AllocationID: allocation.ID, SpecDigest: parent.SpecDigest, Objective: parent.Spec.Objective}
	baseline, candidate = map[string]DecisionEvidence{}, map[string]DecisionEvidence{}
	allIDs, trialIDs := map[string]bool{}, map[id.ID]bool{}
	groupIndexes := map[ContentRef]int{}
	covered := map[string]bool{}
	for index, value := range children {
		child, template, arm := value.Experiment, plan.Children[index], allocation.PairedRoster[index]
		if child.ID != template.ExperimentID || !equalControllerData(child.Spec, template.Spec) || !equalControllerData(child.EvaluationPlan, template.EvaluationPlan) || child.Spec.Parent == nil || child.Spec.Parent.ExperimentID != parent.ID || child.State != StateRunning || child.Phase != PhaseValidate && child.Phase != PhaseDecide || arm.ExperimentID != child.ID || arm.Controller != child.Spec.Controller || arm.Baseline != child.Spec.Baseline {
			return request, nil, nil, ErrIncompleteEvidence
		}
		tasks, digests := map[string]TaskCase{}, map[string]bool{}
		for _, task := range child.Spec.Tasks {
			if task.Split == SplitValidation {
				tasks[task.ID], digests[task.Digest], covered[task.Digest] = task, true, true
			}
		}
		if !sameEvidenceIDs(arm.CaseDigests, digests) || len(value.Trials) != len(tasks)*child.Spec.Limits.Repetitions {
			return request, nil, nil, ErrIncompleteEvidence
		}
		groupIndex, exists := groupIndexes[child.EvaluationPlan.Ref]
		if !exists {
			groupIndex = len(request.Groups)
			groupIndexes[child.EvaluationPlan.Ref] = groupIndex
			request.Groups = append(request.Groups, ControllerComparisonGroup{Plan: child.EvaluationPlan})
		} else if !equalControllerData(request.Groups[groupIndex].Plan, child.EvaluationPlan) {
			return request, nil, nil, ErrIncompleteEvidence
		}
		group := &request.Groups[groupIndex]
		pairs, ids := baseline, &group.BaselineIDs
		if index%2 == 1 {
			pairs, ids = candidate, &group.CandidateIDs
		}
		for _, item := range value.Trials {
			trial := item.Trial
			task, found := tasks[trial.CaseID]
			key := fmt.Sprintf("%s/%d", trial.CaseID, trial.Repetition)
			if !found || trial.CaseDigest != task.Digest || trial.Repetition < 0 || trial.Repetition >= child.Spec.Limits.Repetitions || trial.ExperimentID != child.ID || trial.Version != arm.Finalist || trial.AllocationID != allocation.ID || trial.Split != SplitValidation || !TrialEvidenceReady(trial) || trialIDs[trial.ID] {
				return request, nil, nil, ErrIncompleteEvidence
			}
			if _, duplicate := pairs[key]; duplicate {
				return request, nil, nil, ErrIncompleteEvidence
			}
			pairs[key], trialIDs[trial.ID] = item, true
			if item.Evaluation != nil {
				if item.Evaluation.ID != trial.EvaluationID || item.Evaluation.ValidateCaptured(child, trial) != nil || allIDs[item.Evaluation.ID] {
					return request, nil, nil, ErrIncompleteEvidence
				}
				allIDs[item.Evaluation.ID] = true
				*ids = append(*ids, item.Evaluation.ID)
			} else if trial.EvaluationID != "" || trial.Execution != nil {
				return request, nil, nil, ErrIncompleteEvidence
			}
		}
	}
	if !sameEvidenceIDs(allocation.CaseDigests, covered) {
		return request, nil, nil, ErrIncompleteEvidence
	}
	for index := range request.Groups {
		slices.Sort(request.Groups[index].BaselineIDs)
		slices.Sort(request.Groups[index].CandidateIDs)
	}
	return request, baseline, candidate, nil
}

// ValidateControllerDecisionEvidence checks a controller recommendation against
// original child captures. Statistical intervals remain the evaluator's responsibility.
func ValidateControllerDecisionEvidence(parent Experiment, allocation ValidationAllocation, decision Decision, children []ControllerEvidence) error {
	request, baseline, candidate, err := controllerComparisonEvidence(parent, allocation, children)
	if err != nil {
		return err
	}
	controllers := parent.Spec.Improvement.Controllers
	if decision.ExperimentID != parent.ID || decision.SpecDigest != parent.SpecDigest || decision.AllocationID != allocation.ID || decision.SelectionKind != string(HarnessImprovement) || decision.Baseline != controllers[0] || decision.Candidate != controllers[1] {
		return ErrIncompleteEvidence
	}
	if summaryErr := validateDecisionSummary(parent, decision); summaryErr != nil {
		return summaryErr
	}
	if len(decision.Comparison.Plans) != len(request.Groups) {
		return ErrIncompleteEvidence
	}
	baseIDs, candidateIDs, allIDs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for index, group := range request.Groups {
		if decision.Comparison.Plans[index] != group.Plan.Ref {
			return ErrIncompleteEvidence
		}
		for _, key := range group.BaselineIDs {
			baseIDs[key], allIDs[key] = true, true
		}
		for _, key := range group.CandidateIDs {
			candidateIDs[key], allIDs[key] = true, true
		}
	}
	digest := ""
	if len(request.Groups) == 1 {
		digest = request.Groups[0].Plan.Ref.Digest
	}
	if decision.Comparison.PlanDigest != digest || !sameEvidenceIDs(decision.EvaluationIDs, allIDs) || !sameEvidenceIDs(decision.Comparison.BaselineIDs, baseIDs) || !sameEvidenceIDs(decision.Comparison.CandidateIDs, candidateIDs) {
		return ErrIncompleteEvidence
	}
	stats, err := collectDecisionPairs(parent, baseline, candidate)
	if err != nil {
		return err
	}
	return validateDecisionStatistics(parent, decision, stats)
}

// BuildControllerInconclusive preserves terminal failures without inventing
// scores. A nil result means every selected execution has a complete evaluation
// and the statistical evaluator must perform the comparison.
func BuildControllerInconclusive(parent Experiment, allocation ValidationAllocation, children []ControllerEvidence) (*Comparison, error) {
	request, baseline, candidate, err := controllerComparisonEvidence(parent, allocation, children)
	if err != nil {
		return nil, err
	}
	stats, err := collectDecisionPairs(parent, baseline, candidate)
	if err != nil {
		return nil, err
	}
	if stats.complete {
		return nil, nil
	}
	result := &Comparison{Confidence: request.Objective.Confidence, BootstrapSamples: request.Objective.BootstrapSamples, BootstrapSeed: request.Objective.BootstrapSeed, MissingReasons: []string{"Selected child executions do not all have complete scores."}}
	for _, group := range request.Groups {
		result.Plans = append(result.Plans, group.Plan.Ref)
		result.BaselineIDs = append(result.BaselineIDs, group.BaselineIDs...)
		result.CandidateIDs = append(result.CandidateIDs, group.CandidateIDs...)
	}
	if len(request.Groups) == 1 {
		result.PlanDigest = request.Groups[0].Plan.Ref.Digest
	}
	for failure := range stats.failures {
		result.RequiredFailures = append(result.RequiredFailures, failure)
	}
	slices.Sort(result.RequiredFailures)
	return result, nil
}
