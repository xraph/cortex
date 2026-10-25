package optimization

import (
	"context"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/run"
)

// ImprovementSpec contains a host-frozen parent and its ordered child templates.
// Bundle.Experiment.Spec.Improvement identifies both immutable controller arms.
type ImprovementSpec struct {
	Bundle ExperimentBundle `json:"bundle"`
}

// CreateImprovementExperiment authenticates a frozen comparison before saving
// its draft. It does not start workers or dispatch controller or judge calls.
func (o *Optimizer) CreateImprovementExperiment(ctx context.Context, request ImprovementSpec) (*Experiment, error) {
	if err := allowOptimizationInvocation(ctx); err != nil {
		return nil, err
	}
	bundle, err := o.creationBundle(ctx, request.Bundle)
	if err != nil {
		return nil, err
	}
	experiment := bundle.Experiment
	if experiment.Spec.Improvement == nil || experiment.Spec.Parent != nil || bundle.ParentLease != nil {
		return nil, ErrInvalidSpec
	}
	if validationErr := ValidateSpec(experiment.Spec); validationErr != nil {
		return nil, validationErr
	}
	if validationErr := experiment.EvaluationPlan.Validate(experiment.Spec); validationErr != nil {
		return nil, validationErr
	}
	if authErr := o.authorizeImprovementChildren(ctx, experiment); authErr != nil {
		return nil, authErr
	}
	// Identical frozen plans are shared by the paired arms. Qualify each exact
	// definition once; a reused source with different metadata is a conflict.
	qualified := map[ContentRef]EvaluationPlan{}
	qualify := func(value Experiment) error {
		if previous, exists := qualified[value.EvaluationPlan.Ref]; exists {
			if !equalControllerData(previous, value.EvaluationPlan) {
				return ErrIncompleteEvidence
			}
			return nil
		}
		if authErr := o.authorizeCreation(ctx, experiment); authErr != nil {
			return authErr
		}
		if authErr := o.authorizeImprovementChildren(ctx, experiment); authErr != nil {
			return authErr
		}
		if verifyErr := verifyEvaluationPlan(ctx, o.config.ImprovementEvaluator, value); verifyErr != nil {
			return verifyErr
		}
		qualified[value.EvaluationPlan.Ref] = value.EvaluationPlan
		return nil
	}
	if qualifyErr := qualify(experiment); qualifyErr != nil {
		return nil, qualifyErr
	}
	for _, child := range experiment.Spec.Improvement.Children {
		if qualifyErr := qualify(Experiment{ID: child.ExperimentID, Spec: child.Spec, EvaluationPlan: child.EvaluationPlan}); qualifyErr != nil {
			return nil, qualifyErr
		}
	}
	if authErr := o.authorizeImprovementChildren(ctx, experiment); authErr != nil {
		return nil, authErr
	}
	// Create rechecks the current caller and parent authority after qualification.
	created, err := o.Create(ctx, *bundle)
	if err != nil {
		return nil, err
	}
	return &created, nil
}

func (o *Optimizer) authorizeImprovementChildren(ctx context.Context, parent Experiment) error {
	for _, child := range parent.Spec.Improvement.Children {
		resource := Resource{Scope: copyScope(parent.Spec.Scope), AgentID: parent.Spec.AgentID, ID: child.ExperimentID, Kind: "experiment"}
		if err := o.config.Authorizer.Authorize(copyScopeContext(ctx), ActionCreate, resource); err != nil {
			return err
		}
	}
	return nil
}

func allowOptimizationInvocation(ctx context.Context) error {
	if provenance, ok := engine.ExecutionProvenanceFromContext(ctx); ok && (provenance.Purpose == run.PurposeOptimizationTrial || provenance.Purpose == run.PurposeOptimizationController || provenance.Purpose == run.PurposeEvaluation) {
		return ErrPolicyDenied
	}
	return nil
}
