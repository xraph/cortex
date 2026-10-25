package optimization

import (
	"context"

	"github.com/xraph/sentinel"
	"github.com/xraph/sentinel/comparison"
	sid "github.com/xraph/sentinel/id"

	opt "github.com/xraph/cortex/optimization"
)

var _ opt.ControllerEvaluator = (*Evaluator)(nil)

// CompareControllers reloads the frozen stream and compares its original child
// captures. It cannot execute a task or dispatch a judge call.
func (e *Evaluator) CompareControllers(ctx context.Context, request opt.ControllerComparisonRequest) (opt.Comparison, error) {
	var empty opt.Comparison
	if e.config.Repository == nil {
		return empty, opt.ErrUnavailable
	}
	if request.Objective.Confidence != .95 || request.Objective.BootstrapSamples != 2000 {
		return empty, opt.ErrUnavailable
	}
	parent, err := e.config.Repository.GetExperiment(ctx, request.ExperimentID)
	if err != nil {
		return empty, err
	}
	if parent.State != opt.StateRunning || parent.Phase != opt.PhaseDecide {
		return empty, opt.ErrConflict
	}
	allocation, err := e.config.Repository.GetValidationAllocation(ctx, request.AllocationID)
	if err != nil {
		return empty, err
	}
	evidence, err := opt.ReadControllerEvidence(ctx, e.config.Repository, parent, allocation)
	if err != nil {
		return empty, err
	}
	canonical, err := opt.BuildControllerComparison(parent, allocation, evidence)
	if err != nil {
		return empty, err
	}
	if !equal(canonical, request) {
		return empty, opt.ErrIncompleteEvidence
	}
	sources := map[string]comparisonSource{}
	for _, child := range evidence {
		for _, item := range child.Trials {
			if item.Evaluation != nil {
				sources[item.Evaluation.ID] = comparisonSource{experiment: child.Experiment.ID.String(), candidate: item.Trial.CandidateID.String()}
			}
		}
	}
	groups := make([]comparison.RecordedComparisonGroup, 0, len(request.Groups))
	bindings := map[sid.ID]opt.TaskCase{}
	projected := opt.ComparisonRequest{Objective: request.Objective}
	first, err := e.binding(ctx, request.Groups[0].Plan.Ref)
	if err != nil {
		return empty, err
	}
	mapped, done, err := e.mapped(ctx, first)
	if err != nil {
		return empty, err
	}
	defer done()
	var baseline, candidate []sid.ID
	for _, group := range request.Groups {
		binding, bindErr := e.binding(ctx, group.Plan.Ref)
		if bindErr != nil {
			return empty, bindErr
		}
		if binding.AppID != sentinel.AppFromContext(mapped) || binding.TenantID != sentinel.TenantFromContext(mapped) {
			return empty, opt.ErrPolicyDenied
		}
		frozen, plan, planErr := e.plan(mapped, binding)
		if planErr != nil {
			return empty, planErr
		}
		if !equal(plan, group.Plan) {
			return empty, opt.ErrIncompleteEvidence
		}
		for _, value := range binding.Cases {
			if prior, exists := bindings[value.CaseID]; exists && !equal(prior, value.Task) {
				return empty, opt.ErrIncompleteEvidence
			}
			bindings[value.CaseID] = value.Task
		}
		base, parseErr := parseJobIDs(group.BaselineIDs)
		if parseErr != nil {
			return empty, parseErr
		}
		cand, parseErr := parseJobIDs(group.CandidateIDs)
		if parseErr != nil {
			return empty, parseErr
		}
		groups = append(groups, comparison.RecordedComparisonGroup{Plan: comparison.RecordedPlanRef{ID: frozen.ID, Digest: frozen.Digest}, BaselineIDs: base, CandidateIDs: cand})
		baseline, candidate = append(baseline, base...), append(candidate, cand...)
		projected.BaselineIDs = append(projected.BaselineIDs, group.BaselineIDs...)
		projected.CandidateIDs = append(projected.CandidateIDs, group.CandidateIDs...)
	}
	incomplete, err := opt.BuildControllerInconclusive(parent, allocation, evidence)
	if err != nil {
		return empty, err
	}
	if incomplete != nil {
		return *incomplete, nil
	}
	report, err := comparison.CompareRecordedGroups(mapped, comparison.RecordedGroupComparisonRequest{Store: e.config.Engine.Store(), Groups: groups, Seed: request.Objective.BootstrapSeed, MinCases: request.Objective.MinValidationCases})
	if err != nil {
		return empty, err
	}
	if len(report.Plans) != len(groups) {
		return empty, opt.ErrIncompleteEvidence
	}
	for _, group := range groups {
		found := false
		for _, plan := range report.Plans {
			if group.Plan == plan {
				found = true
				break
			}
		}
		if !found {
			return empty, opt.ErrIncompleteEvidence
		}
	}
	result, err := e.projectComparison(ctx, mapped, projected, report, bindings, baseline, candidate, sources)
	if err != nil {
		return empty, err
	}
	for _, group := range request.Groups {
		result.Plans = append(result.Plans, group.Plan.Ref)
	}
	return result, nil
}
