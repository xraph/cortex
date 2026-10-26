package optimization

import (
	"context"
	"slices"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
)

// CatalogAuthorizer checks the current caller's evaluation grant in the mapped
// Sentinel app and tenant. Return ErrPolicyDenied to exclude a catalog choice;
// return an availability error when the grant cannot be checked. Scope mapping
// alone does not grant access. Implementations must preserve the context identity.
type CatalogAuthorizer interface {
	AuthorizeEvaluation(context.Context, opt.ContentRef) error
}

// CatalogAccess qualifies registered evaluation choices without running targets
// or judges. Use it with ApprovedCatalog and your host's creation preparation.
type CatalogAccess struct {
	evaluator  *Evaluator
	authorizer CatalogAuthorizer
}

// NewCatalogAccess requires explicit current-grant checks for private plan reads.
func NewCatalogAccess(evaluator *Evaluator, authorizer CatalogAuthorizer) (*CatalogAccess, error) {
	if evaluator == nil || evaluator.config.Engine == nil || authorizer == nil {
		return nil, opt.ErrInvalidSpec
	}
	return &CatalogAccess{evaluator: evaluator, authorizer: authorizer}, nil
}

// Authorize checks an evaluation choice against the exact current scope, mapped
// source grant and frozen plan. Other catalog kinds need their host's authorizer.
func (a *CatalogAccess) Authorize(ctx context.Context, choice opt.CatalogChoice) error {
	if a == nil || a.evaluator == nil || a.authorizer == nil {
		return opt.ErrUnavailable
	}
	scope, err := opt.ScopeKey(cortex.ScopeFromContext(ctx))
	if err != nil {
		return err
	}
	selected, err := opt.ScopeKey(choice.Scope)
	if err != nil || choice.Kind != opt.CatalogEvaluation || selected != scope {
		return opt.ErrPolicyDenied
	}
	binding, err := a.evaluator.binding(ctx, choice.Ref)
	if err != nil {
		return err
	}
	tasks := make([]opt.TaskCase, 0, len(binding.Cases))
	for _, item := range binding.Cases {
		tasks = append(tasks, item.Task)
	}
	_, err = a.Freeze(ctx, opt.EvaluationSpec{Source: choice.Ref, Tasks: tasks})
	return err
}

// Freeze checks the mapped source grant before reading private plan content and
// again before returning the qualified plan. It shares the evaluator's task,
// criterion and scorer revision checks. No evaluation is dispatched.
func (a *CatalogAccess) Freeze(ctx context.Context, spec opt.EvaluationSpec) (opt.EvaluationPlan, error) {
	if a == nil || a.evaluator == nil || a.authorizer == nil {
		return opt.EvaluationPlan{}, opt.ErrUnavailable
	}
	return a.evaluator.freeze(ctx, spec, func(mapped context.Context) error {
		return a.authorizer.AuthorizeEvaluation(mapped, spec.Source)
	})
}

func detachedScope(ctx context.Context) context.Context {
	scope := cortex.ScopeFromContext(ctx)
	scope.Levels = slices.Clone(scope.Levels)
	return cortex.WithScope(ctx, scope)
}
