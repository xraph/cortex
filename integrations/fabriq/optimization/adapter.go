// Package optimization connects governed Cortex lessons to Fabriq.
package optimization

import (
	"context"
	"time"

	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"
	"github.com/xraph/fabriq/core/query"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
)

const recallTokenBudget = 128000

// Toolkit is the guarded Fabriq command and recall boundary.
type Toolkit interface {
	Remember(context.Context, agent.RememberRequest) (command.Result, error)
	Recall(context.Context, agent.RecallRequest) (agent.ContextPack, error)
}

// ScopeBinding maps one authenticated Cortex scope into a Fabriq tenant.
type ScopeBinding struct {
	Tenant string
	Scope  cortex.Scope
}

// ScopeMapper resolves a tenant without changing the caller's exact Cortex scope.
type ScopeMapper interface {
	Map(context.Context, cortex.Scope) (ScopeBinding, error)
}

// ScopeMapperFunc adapts a host mapping function.
type ScopeMapperFunc func(context.Context, cortex.Scope) (ScopeBinding, error)

// Map delegates to the host mapping function.
func (f ScopeMapperFunc) Map(ctx context.Context, s cortex.Scope) (ScopeBinding, error) {
	return f(ctx, s)
}

// Config supplies local evidence and current host authority.
type Config struct {
	Entity           string
	Repository       opt.Repository
	Authorizer       opt.Authorizer
	IdentityResolver opt.IdentityResolver
	Now              func() time.Time
}

// Adapter implements governed lesson delivery and recall.
type Adapter struct {
	toolkit Toolkit
	fabric  query.Fabric
	mapper  ScopeMapper
	config  Config
}

// NewLessonStore constructs the adapter without starting background work.
func NewLessonStore(toolkit Toolkit, fabric query.Fabric, mapper ScopeMapper, config Config) (*Adapter, error) {
	if toolkit == nil || fabric == nil || mapper == nil || config.Repository == nil || config.Authorizer == nil || config.IdentityResolver == nil || !validEntity(config.Entity) {
		return nil, opt.ErrInvalidSpec
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Adapter{toolkit: toolkit, fabric: fabric, mapper: mapper, config: config}, nil
}
