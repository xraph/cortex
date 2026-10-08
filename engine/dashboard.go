package engine

import (
	"context"
	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
)

// VisibleTools returns the same scoped definitions a run may advertise.
// Reading the registry alone would omit builtin/external tools and bypass the
// host's visibility policy. The caller must already be authorized to read the agent.
func (e *Engine) VisibleTools(ctx context.Context, agentID id.AgentID) ([]llm.Tool, error) {
	ag, err := e.GetAgent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	ctx = cortex.WithScope(ctx, ag.Scope)
	overlays, err := e.loadScopeOverlays(ctx, ag.ID)
	if err != nil {
		return nil, err
	}
	cfg := e.effectiveConfig(ag, nil, overlays)
	subject := cortex.Subject{Scope: cortex.ScopeFromContext(ctx), Principal: cortex.PrincipalFromContext(ctx), AgentID: ag.ID}
	return e.resolveTools(ctx, subject, cfg.Tools, cfg.ToolsRestricted), nil
}

// HasToolAuthorizer distinguishes host policy from the permissive default.
func (e *Engine) HasToolAuthorizer() bool { return e.authorizer != nil }
