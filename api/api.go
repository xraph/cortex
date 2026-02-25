// Package api provides Forge-style HTTP handlers for the Cortex agent system.
package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/forge"

	"github.com/xraph/cortex/engine"
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
	if err := a.RegisterRoutes(a.router); err != nil {
		panic(fmt.Sprintf("register cortex routes: %v", err))
	}
	return a.router.Handler()
}

// RegisterRoutes registers all Cortex API routes into the given Forge router
// with full OpenAPI metadata.
func (a *API) RegisterRoutes(router forge.Router) error {
	if err := a.registerAgentRoutes(router); err != nil {
		return err
	}
	if err := a.registerRunRoutes(router); err != nil {
		return err
	}
	if err := a.registerSkillRoutes(router); err != nil {
		return err
	}
	if err := a.registerTraitRoutes(router); err != nil {
		return err
	}
	if err := a.registerBehaviorRoutes(router); err != nil {
		return err
	}
	if err := a.registerPersonaRoutes(router); err != nil {
		return err
	}
	if err := a.registerCheckpointRoutes(router); err != nil {
		return err
	}
	if err := a.registerMemoryRoutes(router); err != nil {
		return err
	}
	return a.registerToolRoutes(router)
}
