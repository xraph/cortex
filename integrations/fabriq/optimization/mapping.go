package optimization

import (
	"context"

	"github.com/xraph/fabriq/core/tenant"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/integrations/fabriq/internal/lessontransport"
	"github.com/xraph/cortex/learning"
	opt "github.com/xraph/cortex/optimization"
)

type lessonRow = lessontransport.Row

func sharedPublication(p opt.Publication) learning.Publication {
	return learning.Publication{LessonID: p.LessonID, Revision: p.LocalRevision, SourceID: p.ExperimentID, Operation: string(p.Operation), Payload: p.Payload, PayloadDigest: p.PayloadDigest, AggregateID: p.RemoteAggregateID}
}
func rowFor(p opt.Publication) lessonRow { return lessontransport.For(sharedPublication(p)) }
func decodeRow(raw map[string]any) (lessonRow, error) {
	row, err := lessontransport.Decode(raw)
	if err != nil {
		return lessonRow{}, opt.ErrIncompleteEvidence
	}
	return row, nil
}

func (a *Adapter) mapped(ctx context.Context) (context.Context, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	key, err := opt.ScopeKey(scope)
	if err != nil {
		return nil, err
	}
	detached := cortex.Scope{Levels: append([]cortex.Level(nil), scope.Levels...)}
	binding, err := a.mapper.Map(ctx, detached)
	if err != nil {
		return nil, err
	}
	mappedKey, err := opt.ScopeKey(binding.Scope)
	if err != nil || mappedKey != key {
		return nil, opt.ErrPolicyDenied
	}
	if current, currentErr := tenant.FromContext(ctx); currentErr == nil && current != binding.Tenant {
		return nil, opt.ErrPolicyDenied
	}
	if !tenant.Valid(binding.Tenant) {
		return nil, opt.ErrPolicyDenied
	}
	return tenant.WithTenant(ctx, binding.Tenant)
}

func (a *Adapter) authorize(ctx context.Context, p opt.Publication, action opt.Action) (context.Context, context.CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	scope := cortex.ScopeFromContext(ctx)
	key, err := opt.ScopeKey(scope)
	if err != nil {
		return nil, nil, err
	}
	if key != p.Payload.Scope.Canonical() {
		return nil, nil, opt.ErrPolicyDenied
	}
	experiment, err := a.config.Repository.GetExperiment(ctx, p.ExperimentID)
	if err != nil {
		return nil, nil, err
	}
	if experiment.ID != p.ExperimentID || experiment.Spec.Scope.Canonical() != key {
		return nil, nil, opt.ErrPolicyDenied
	}
	resource := opt.Resource{Scope: scope, AgentID: experiment.Spec.AgentID, ID: p.LessonID, Kind: "lesson"}
	if authErr := a.config.Authorizer.Authorize(ctx, action, resource); authErr != nil {
		return nil, nil, authErr
	}
	resolved, cleanup, err := opt.ResolveIdentity(ctx, a.config.IdentityResolver, experiment.Spec.Requester, scope)
	if err != nil {
		return nil, nil, err
	}
	if err = a.config.Authorizer.Authorize(resolved, action, resource); err != nil {
		cleanup()
		return nil, nil, err
	}
	mapped, err := a.mapped(resolved)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return mapped, cleanup, nil
}
