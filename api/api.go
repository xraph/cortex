// Package api provides Forge-style HTTP handlers for the Cortex agent system.
package api

import (
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/xraph/forge"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/optimization"
)

// API wires all Forge-style HTTP handlers together for the Cortex system.
type API struct {
	replayStreams atomic.Int64
	contentReads  atomic.Int64
	eng           *engine.Engine
	optimization  *optimization.Optimizer
	planning      *coordination.PlanningOperator
	router        forge.Router
}

// New creates an API from a Cortex Engine.
func New(eng *engine.Engine, router forge.Router, options ...Option) *API {
	a := &API{eng: eng, router: router}
	for _, option := range options {
		if option != nil {
			option(a)
		}
	}
	return a
}

// Option configures optional API services without starting background work.
type Option func(*API)

// WithOptimization exposes scoped operator workflows through the supplied service.
// The host supplies the optimizer's authenticated caller and current authorizer.
func WithOptimization(value *optimization.Optimizer) Option {
	return func(a *API) { a.optimization = value }
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
	if err := a.registerSessionRoutes(router); err != nil {
		return err
	}
	if err := a.registerToolRoutes(router); err != nil {
		return err
	}
	if err := a.registerOrchestrationRoutes(router); err != nil {
		return err
	}
	if err := a.registerA2ARoutes(router); err != nil {
		return err
	}
	if err := a.registerConfigRoutes(router); err != nil {
		return err
	}
	if err := a.registerPlanningRoutes(router); err != nil {
		return err
	}
	return a.registerOptimizationRoutes(router)
}
