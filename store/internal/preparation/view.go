// Package preparation binds snapshot readers to a scope and callback lifetime.
package preparation

import (
	"context"
	"sync"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/trait"
)

// View exposes only preparation reads and serializes access to a transaction.
type View struct {
	mu     sync.Mutex
	source store.PreparationSources
	ctx    context.Context
	cancel context.CancelFunc
	scope  string
	bind   func(context.Context) context.Context
	closed bool
}

// New creates a callback-lifetime view. bind attaches a backend session, if any.
func New(ctx context.Context, source store.PreparationSources, bind func(context.Context) context.Context) *View {
	bound, cancel := context.WithCancel(ctx)
	return &View{source: source, ctx: bound, cancel: cancel, scope: cortex.ScopeFromContext(ctx).Canonical(), bind: bind}
}

// Close invalidates the view before the backend releases its transaction.
func (v *View) Close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.closed = true
	v.cancel()
}

func read[T any](ctx context.Context, v *View, query func(context.Context) (*T, error)) (*T, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return nil, cortex.ErrPreparationUnavailable
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	if scope.Canonical() != v.scope {
		return nil, cortex.ErrInvalidScope
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if captureErr := v.ctx.Err(); captureErr != nil {
		return nil, captureErr
	}
	queryCtx, cancel := context.WithCancel(v.ctx)
	stop := context.AfterFunc(ctx, cancel)
	defer func() { stop(); cancel() }()
	if v.bind != nil {
		queryCtx = v.bind(queryCtx)
	}
	return query(queryCtx)
}

// Get reads an agent from the bound snapshot.
func (v *View) Get(ctx context.Context, key id.AgentID) (*agent.Config, error) {
	return read(ctx, v, func(c context.Context) (*agent.Config, error) { return v.source.Get(c, key) })
}

// GetPersonaByName reads a persona from the bound snapshot.
func (v *View) GetPersonaByName(ctx context.Context, name string) (*persona.Persona, error) {
	return read(ctx, v, func(c context.Context) (*persona.Persona, error) { return v.source.GetPersonaByName(c, name) })
}

// GetSkillByName reads a skill from the bound snapshot.
func (v *View) GetSkillByName(ctx context.Context, name string) (*skill.Skill, error) {
	return read(ctx, v, func(c context.Context) (*skill.Skill, error) { return v.source.GetSkillByName(c, name) })
}

// GetTraitByName reads a trait from the bound snapshot.
func (v *View) GetTraitByName(ctx context.Context, name string) (*trait.Trait, error) {
	return read(ctx, v, func(c context.Context) (*trait.Trait, error) { return v.source.GetTraitByName(c, name) })
}

// GetOverlayForAgentAt reads an ancestor overlay from the bound snapshot.
func (v *View) GetOverlayForAgentAt(ctx context.Context, key id.AgentID, scope cortex.Scope) (*prompt.Overlay, error) {
	return read(ctx, v, func(c context.Context) (*prompt.Overlay, error) { return v.source.GetOverlayForAgentAt(c, key, scope) })
}
