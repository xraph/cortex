package optimization

import (
	"context"

	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"
	"github.com/xraph/fabriq/core/query"
	"github.com/xraph/fabriq/core/registry"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	opt "github.com/xraph/cortex/optimization"
)

// LessonToolkit owns an immutable lesson write policy and strict semantic recall.
// Embedding this type in a decorator preserves its policy attestation.
type LessonToolkit struct {
	toolkit *agent.Toolkit
	entity  string
}

// NewLessonToolkit binds the lesson write policy and semantic retrieval configuration.
func NewLessonToolkit(fabric query.Fabric, reg *registry.Registry, embedder agent.Embedder, entity string) (*LessonToolkit, error) {
	if fabric == nil || reg == nil || embedder == nil || !validEntity(entity) {
		return nil, opt.ErrInvalidSpec
	}
	registered, ok := reg.Get(entity)
	if !ok || registered.Spec.Schema == nil || registered.Spec.Kind != registry.KindAggregate || registered.Spec.Embed == nil || len(registered.Spec.Embed.Fields) != 1 || registered.Spec.Embed.Fields[0] != "content" {
		return nil, opt.ErrInvalidSpec
	}
	for _, required := range LessonSpec(entity).Schema.Columns {
		found := false
		for _, column := range registered.Spec.Schema.Columns {
			if column.Name == required.Name && column.Type == required.Type && column.NotNull == required.NotNull {
				found = true
				break
			}
		}
		if !found {
			return nil, opt.ErrInvalidSpec
		}
	}
	toolkit, err := agent.NewToolkit(fabric, reg, embedder, agent.Config{Strict: true, VectorDims: embedder.Dims(), Write: LessonWritePolicy(entity)})
	if err != nil {
		return nil, err
	}
	return &LessonToolkit{toolkit: toolkit, entity: entity}, nil
}

// Remember delegates to the configured guarded command plane.
func (t *LessonToolkit) Remember(ctx context.Context, request agent.RememberRequest) (command.Result, error) {
	if t == nil || t.toolkit == nil {
		return command.Result{}, opt.ErrUnavailable
	}
	return t.toolkit.Remember(ctx, request)
}

// Recall delegates to strict semantic retrieval.
func (t *LessonToolkit) Recall(ctx context.Context, request agent.RecallRequest) (agent.ContextPack, error) {
	if t == nil || t.toolkit == nil {
		return agent.ContextPack{}, opt.ErrUnavailable
	}
	return t.toolkit.Recall(ctx, request)
}

func (t *LessonToolkit) lessonWritePolicy(entity string) bool {
	return t != nil && t.toolkit != nil && entity == t.entity
}

// CheckReady verifies provisioned columns, the bound toolkit policy and semantic
// retrieval without writing a lesson. Destination write grants are exercised by
// actual delivery; this probe does not claim a test write occurred.
func (a *Adapter) CheckReady(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	bound, ok := a.toolkit.(interface{ lessonWritePolicy(string) bool })
	if !ok || !bound.lessonWritePolicy(a.config.Entity) {
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
