package optimization

import "context"

func verifyEvaluationPlan(ctx context.Context, evaluator Evaluator, experiment Experiment) error {
	if evaluator == nil {
		return ErrUnavailable
	}
	request, err := copyModelJSON(&EvaluationSpec{Source: experiment.Spec.Evaluation, Tasks: experiment.Spec.Tasks, RequiredCriteria: experiment.Spec.Objective.RequiredCriteria}, 16<<20)
	if err != nil {
		return err
	}
	plan, err := evaluator.Freeze(copyScopeContext(ctx), *request)
	if err != nil {
		return err
	}
	if validationErr := plan.Validate(experiment.Spec); validationErr != nil {
		return validationErr
	}
	if !equalControllerData(plan, experiment.EvaluationPlan) {
		return ErrIncompleteEvidence
	}
	return nil
}
