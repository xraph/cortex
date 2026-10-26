package optimization

import (
	"context"

	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/query"
	"github.com/xraph/fabriq/core/registry"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/fabriq/internal/lessontransport"
	opt "github.com/xraph/cortex/optimization"
)

// LessonToolkit binds the shared typed schema and guarded recall policy.
type LessonToolkit = lessontransport.Toolkit

// NewLessonToolkit constructs strict recall with an explicit write policy.
func NewLessonToolkit(fabric query.Fabric, reg *registry.Registry, embedder agent.Embedder, entity string) (*LessonToolkit, error) {
	toolkit, err := lessontransport.NewToolkit(fabric, reg, embedder, entity)
	if err != nil {
		return nil, opt.ErrInvalidSpec
	}
	return toolkit, nil
}

// CheckReady verifies provisioned columns, the bound toolkit policy and semantic
// retrieval without writing a lesson. Destination write grants are exercised by
// actual delivery; this probe does not claim a test write occurred.
func (a *Adapter) CheckReady(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !lessontransport.BoundPolicy(a.toolkit, a.config.Entity) {
		return opt.ErrUnavailable
	}
	scope := cortex.ScopeFromContext(ctx)
	key, err := opt.ScopeKey(scope)
	if err != nil {
		return err
	}
	for _, action := range []opt.Action{opt.ActionRead, opt.ActionPublishLesson, opt.ActionRevokeLesson} {
		if authErr := a.config.Authorizer.Authorize(ctx, action, opt.Resource{Scope: scope, Kind: "lesson"}); authErr != nil {
			return authErr
		}
	}
	mapped, err := a.mapped(ctx)
	if err != nil {
		return err
	}
	filters := query.Where{query.Eq("scope_key", key)}
	for _, column := range LessonSpec(a.config.Entity).Schema.Columns {
		filters = append(filters, query.Or(query.IsNull(column.Name), query.IsNotNull(column.Name)))
	}
	var rows []map[string]any
	if err = a.fabric.Relational().List(mapped, a.config.Entity, query.ListQuery{Where: filters, Limit: 1}, &rows); err != nil {
		return opt.ErrUnavailable
	}
	// The fresh ID avoids returning lesson content while still exercising vector
	// retrieval and authoritative hydration through the configured schema.
	filters = append(filters, query.Eq("id", id.New(opt.PrefixLesson).String()))
	pack, err := a.toolkit.Recall(mapped, agent.RecallRequest{Query: "optimization lesson readiness", Budget: recallTokenBudget, K: 1, Entities: []string{a.config.Entity}, Filters: filters})
	if err != nil || len(pack.Warnings) > 0 || pack.Omitted > 0 || len(pack.Items) > 0 {
		return opt.ErrUnavailable
	}
	return ctx.Err()
}
