package optimization

import (
	"context"
	"math"

	"github.com/xraph/sentinel/recorded"

	opt "github.com/xraph/cortex/optimization"
)

// executionCost joins Sentinel's immutable job to the original Cortex capture.
// Currency alone cannot establish compatible pricing or a measured charge.
func (e *Evaluator) executionCost(ctx context.Context, job *recorded.Job) (cost int64, known bool, err error) {
	if e.config.Repository == nil {
		return 0, false, nil
	}
	evaluation, err := e.config.Repository.GetEvaluation(ctx, job.ID.String())
	if err != nil {
		return 0, false, err
	}
	trial, err := e.config.Repository.GetTrial(ctx, evaluation.TrialID)
	if err != nil {
		return 0, false, err
	}
	experiment, err := e.config.Repository.GetExperiment(ctx, evaluation.ExperimentID)
	if err != nil {
		return 0, false, err
	}
	if trial.ID != evaluation.TrialID || trial.Execution == nil || trial.EvaluationID != evaluation.ID || evaluation.ID != job.ID.String() || evaluation.ReportID != job.RunID.String() || trial.ExperimentID != evaluation.ExperimentID || experiment.ID.String() != job.Request.SourceExperimentID || trial.CandidateID.String() != job.Request.SourceCandidateID || evaluation.ExecutionDigest != trial.Execution.Digest || evaluation.PlanDigest != job.Request.PlanDigest || experiment.EvaluationPlan.Ref.Digest != job.Request.PlanDigest || len(job.Request.Cases) != 1 {
		return 0, false, opt.ErrIncompleteEvidence
	}
	capture, err := ConvertExecution(*trial.Execution)
	if err != nil {
		return 0, false, err
	}
	stored := job.Request.Cases[0]
	binding, bindingErr := e.binding(ctx, experiment.EvaluationPlan.Ref)
	if bindingErr != nil {
		return 0, false, bindingErr
	}
	matched := false
	for _, pair := range binding.Cases {
		if pair.Task.ID != trial.CaseID || pair.Task.Digest != trial.CaseDigest || pair.Task.InputDigest != trial.Execution.InputDigest || pair.CaseID != stored.CaseID {
			continue
		}
		capture.CaseID = pair.CaseID
		capture.InputDigest = recorded.InputDigest(pair.Task.Input)
		capture.Repetition = trial.Repetition
		matched = true
		break
	}
	if !matched {
		return 0, false, opt.ErrIncompleteEvidence
	}
	if !equal(capture, stored) || evaluation.CaseID != trial.CaseID || evaluation.CaseDigest != trial.CaseDigest || evaluation.Repetition != trial.Repetition || trial.Repetition != stored.Repetition {
		return 0, false, opt.ErrIncompleteEvidence
	}
	cost, known = trial.Execution.Usage.KnownCost()
	return cost, known && !experiment.Spec.Budget.TokenOnly && trial.Execution.Usage.PricingRevision == experiment.Spec.Budget.PricingRevision, nil
}

func addCost(total, cost int64) (int64, error) {
	if cost < 0 || total > math.MaxInt64-cost {
		return 0, opt.ErrIncompleteEvidence
	}
	return total + cost, nil
}
