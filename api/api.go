// Package api provides Forge-style HTTP handlers for the Cortex agent system.
package api

import (
	"net/http"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/forge"
)

// API wires all Forge-style HTTP handlers together for the Cortex system.
type API struct {
	eng    *engine.Engine
	router forge.Router
}

// New creates an API from a Cortex Engine.
func New(eng *engine.Engine, router forge.Router) *API {
	return &API{eng: eng, router: router}
}

// Handler returns the fully assembled http.Handler with all routes.
func (a *API) Handler() http.Handler {
	if a.router == nil {
		a.router = forge.NewRouter()
	}
	a.RegisterRoutes(a.router)
	return a.router.Handler()
}

// RegisterRoutes registers all Cortex API routes into the given Forge router
// with full OpenAPI metadata.
func (a *API) RegisterRoutes(router forge.Router) {
	a.registerAgentRoutes(router)
	a.registerRunRoutes(router)
	a.registerSkillRoutes(router)
	a.registerTraitRoutes(router)
	a.registerBehaviorRoutes(router)
	a.registerPersonaRoutes(router)
	a.registerCheckpointRoutes(router)
	a.registerMemoryRoutes(router)
	a.registerToolRoutes(router)
}
