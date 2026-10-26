package lessontransport

import (
	"context"

	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"
	"github.com/xraph/fabriq/core/query"
	"github.com/xraph/fabriq/core/registry"

	"github.com/xraph/cortex/learning"
)

// Toolkit owns an immutable lesson write policy and strict semantic recall.
// Embedding this type in a decorator preserves its policy attestation.
type Toolkit struct {
	toolkit *agent.Toolkit
	entity  string
}

// NewToolkit binds the lesson write policy and semantic retrieval configuration.
func NewToolkit(fabric query.Fabric, reg *registry.Registry, embedder agent.Embedder, entity string) (*Toolkit, error) {
	if fabric == nil || reg == nil || embedder == nil || !ValidEntity(entity) {
		return nil, learning.ErrInvalid
	}
	registered, ok := reg.Get(entity)
	if !ok || registered.Spec.Schema == nil || registered.Spec.Kind != registry.KindAggregate || registered.Spec.Embed == nil || len(registered.Spec.Embed.Fields) != 1 || registered.Spec.Embed.Fields[0] != "content" {
		return nil, learning.ErrInvalid
	}
	for _, required := range Spec(entity).Schema.Columns {
		found := false
		for _, column := range registered.Spec.Schema.Columns {
			if column.Name == required.Name && column.Type == required.Type && column.NotNull == required.NotNull {
				found = true
				break
			}
		}
		if !found {
			return nil, learning.ErrInvalid
		}
	}
	toolkit, err := agent.NewToolkit(fabric, reg, embedder, agent.Config{Strict: true, VectorDims: embedder.Dims(), Write: WritePolicy(entity)})
	if err != nil {
		return nil, err
	}
	return &Toolkit{toolkit: toolkit, entity: entity}, nil
}

// Remember delegates to the configured guarded command plane.
func (t *Toolkit) Remember(ctx context.Context, request agent.RememberRequest) (command.Result, error) {
	if t == nil || t.toolkit == nil {
		return command.Result{}, learning.ErrUnavailable
	}
	return t.toolkit.Remember(ctx, request)
}

// Recall delegates to strict semantic retrieval.
func (t *Toolkit) Recall(ctx context.Context, request agent.RecallRequest) (agent.ContextPack, error) {
	if t == nil || t.toolkit == nil {
		return agent.ContextPack{}, learning.ErrUnavailable
	}
	return t.toolkit.Recall(ctx, request)
}

func (t *Toolkit) lessonWritePolicy(entity string) bool {
	return t != nil && t.toolkit != nil && entity == t.entity
}

// BoundPolicy recognizes the private policy binding of this toolkit or a decorator.
func BoundPolicy(toolkit any, entity string) bool {
	bound, ok := toolkit.(interface{ lessonWritePolicy(string) bool })
	return ok && bound.lessonWritePolicy(entity)
}
