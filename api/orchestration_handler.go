package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/forge"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func (a *API) registerOrchestrationRoutes(router forge.Router) error {
	g := router.Group("/v1", forge.WithGroupTags("orchestrations"))

	if err := g.POST("/orchestrations", a.createOrchestration,
		forge.WithSummary("Create orchestration"),
		forge.WithDescription("Creates a multi-agent orchestration config (strategy + participants + settings)."),
		forge.WithOperationID("createOrchestration"),
		forge.WithRequestSchema(CreateOrchestrationRequest{}),
		forge.WithCreatedResponse(&orchestration.Config{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register orchestration routes: %w", err)
	}

	if err := g.GET("/orchestrations", a.listOrchestrations,
		forge.WithSummary("List orchestrations"),
		forge.WithOperationID("listOrchestrations"),
		forge.WithRequestSchema(ListOrchestrationsRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Orchestration list", []*orchestration.Config{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register orchestration routes: %w", err)
	}

	if err := g.GET("/orchestrations/:name", a.getOrchestration,
		forge.WithSummary("Get orchestration"),
		forge.WithOperationID("getOrchestration"),
		forge.WithResponseSchema(http.StatusOK, "Orchestration details", &orchestration.Config{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register orchestration routes: %w", err)
	}

	if err := g.PUT("/orchestrations/:name", a.updateOrchestration,
		forge.WithSummary("Update orchestration"),
		forge.WithOperationID("updateOrchestration"),
		forge.WithRequestSchema(UpdateOrchestrationRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Updated orchestration", &orchestration.Config{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register orchestration routes: %w", err)
	}

	if err := g.DELETE("/orchestrations/:name", a.deleteOrchestration,
		forge.WithSummary("Delete orchestration"),
		forge.WithOperationID("deleteOrchestration"),
		forge.WithNoContentResponse(),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register orchestration routes: %w", err)
	}

	if err := g.POST("/orchestrations/:name/run", a.runOrchestration,
		forge.WithSummary("Run orchestration"),
		forge.WithDescription("Executes a stored orchestration and returns the run summary."),
		forge.WithOperationID("runOrchestration"),
		forge.WithRequestSchema(RunOrchestrationRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Run summary", &RunOrchestrationResponse{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register orchestration routes: %w", err)
	}

	if err := g.GET("/orchestration-runs", a.listOrchestrationRuns,
		forge.WithSummary("List orchestration runs"),
		forge.WithOperationID("listOrchestrationRuns"),
		forge.WithRequestSchema(ListOrchestrationRunsRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Run list", []*orchestration.Run{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register orchestration routes: %w", err)
	}

	if err := g.GET("/orchestration-runs/:id", a.getOrchestrationRun,
		forge.WithSummary("Get orchestration run"),
		forge.WithOperationID("getOrchestrationRun"),
		forge.WithResponseSchema(http.StatusOK, "Run details", &orchestration.Run{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register orchestration routes: %w", err)
	}

	return nil
}

var knownStrategies = map[string]bool{
	orchestration.StrategySequential:   true,
	orchestration.StrategyParallel:     true,
	orchestration.StrategyRouter:       true,
	orchestration.StrategyHierarchical: true,
	orchestration.StrategyDebate:       true,
}

func (a *API) createOrchestration(ctx forge.Context, req *CreateOrchestrationRequest) (*orchestration.Config, error) {
	if req.Name == "" {
		return nil, forge.BadRequest("name is required")
	}
	if !knownStrategies[req.Strategy] {
		return nil, forge.BadRequest("strategy must be one of: sequential, parallel, router, hierarchical, debate")
	}
	if len(req.Participants) == 0 {
		return nil, forge.BadRequest("at least one participant is required")
	}

	c := &orchestration.Config{
		Entity:       cortex.NewEntity(),
		ID:           id.NewOrchestrationConfigID(),
		Name:         req.Name,
		Description:  req.Description,
		AppID:        cortex.AppFromContext(ctx.Context()),
		Strategy:     req.Strategy,
		Participants: req.Participants,
		Settings:     req.Settings,
		Metadata:     req.Metadata,
	}
	if err := a.eng.CreateOrchestration(ctx.Context(), c); err != nil {
		return nil, fmt.Errorf("create orchestration: %w", err)
	}
	return c, ctx.JSON(http.StatusCreated, c)
}

func (a *API) getOrchestration(ctx forge.Context, _ *GetOrchestrationRequest) (*orchestration.Config, error) {
	c, err := a.eng.GetOrchestrationByName(ctx.Context(), cortex.AppFromContext(ctx.Context()), ctx.Param("name"))
	if err != nil {
		return nil, mapStoreError(err)
	}
	return c, ctx.JSON(http.StatusOK, c)
}

func (a *API) listOrchestrations(ctx forge.Context, req *ListOrchestrationsRequest) (*ListOrchestrationsResponse, error) {
	items, err := a.eng.ListOrchestrations(ctx.Context(), &orchestration.ConfigListFilter{
		AppID:  cortex.AppFromContext(ctx.Context()),
		Limit:  defaultLimit(req.Limit),
		Offset: req.Offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list orchestrations: %w", err)
	}
	resp := &ListOrchestrationsResponse{Items: items}
	return resp, ctx.JSON(http.StatusOK, resp)
}

func (a *API) updateOrchestration(ctx forge.Context, req *UpdateOrchestrationRequest) (*orchestration.Config, error) {
	c, err := a.eng.GetOrchestrationByName(ctx.Context(), cortex.AppFromContext(ctx.Context()), req.Name)
	if err != nil {
		return nil, mapStoreError(err)
	}

	if req.Description != "" {
		c.Description = req.Description
	}
	if req.Strategy != "" {
		if !knownStrategies[req.Strategy] {
			return nil, forge.BadRequest("strategy must be one of: sequential, parallel, router, hierarchical, debate")
		}
		c.Strategy = req.Strategy
	}
	if req.Participants != nil {
		c.Participants = req.Participants
	}
	if req.Settings != nil {
		c.Settings = *req.Settings
	}
	if req.Metadata != nil {
		c.Metadata = req.Metadata
	}

	if err := a.eng.UpdateOrchestration(ctx.Context(), c); err != nil {
		return nil, fmt.Errorf("update orchestration: %w", err)
	}
	return c, ctx.JSON(http.StatusOK, c)
}

func (a *API) deleteOrchestration(ctx forge.Context, _ *DeleteOrchestrationRequest) (*struct{}, error) {
	c, err := a.eng.GetOrchestrationByName(ctx.Context(), cortex.AppFromContext(ctx.Context()), ctx.Param("name"))
	if err != nil {
		return nil, mapStoreError(err)
	}
	if err := a.eng.DeleteOrchestration(ctx.Context(), c.ID); err != nil {
		return nil, mapStoreError(err)
	}
	return nil, ctx.NoContent(http.StatusNoContent)
}

func (a *API) runOrchestration(ctx forge.Context, req *RunOrchestrationRequest) (*RunOrchestrationResponse, error) {
	if req.Input == "" {
		return nil, forge.BadRequest("input is required")
	}
	appID := cortex.AppFromContext(ctx.Context())
	r, err := a.eng.RunOrchestration(ctx.Context(), appID, req.Name, req.Input)
	if r == nil {
		return nil, mapStoreError(err)
	}

	var durationMs int64
	if r.CompletedAt != nil {
		durationMs = r.CompletedAt.Sub(r.StartedAt).Milliseconds()
	}
	resp := &RunOrchestrationResponse{
		RunID:      r.ID.String(),
		Status:     r.Status,
		Strategy:   r.Strategy,
		Output:     r.Output,
		DurationMs: durationMs,
	}
	return resp, ctx.JSON(http.StatusOK, resp)
}

func (a *API) listOrchestrationRuns(ctx forge.Context, req *ListOrchestrationRunsRequest) (*ListOrchestrationRunsResponse, error) {
	items, err := a.eng.ListOrchestrationRuns(ctx.Context(), &orchestration.RunListFilter{
		AppID:  cortex.AppFromContext(ctx.Context()),
		Status: req.Status,
		Limit:  defaultLimit(req.Limit),
		Offset: req.Offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list orchestration runs: %w", err)
	}
	resp := &ListOrchestrationRunsResponse{Items: items}
	return resp, ctx.JSON(http.StatusOK, resp)
}

func (a *API) getOrchestrationRun(ctx forge.Context, _ *GetOrchestrationRunRequest) (*orchestration.Run, error) {
	runID, err := id.ParseOrchestrationID(ctx.Param("id"))
	if err != nil {
		return nil, forge.BadRequest("invalid orchestration run id")
	}
	r, err := a.eng.GetOrchestrationRun(ctx.Context(), runID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return r, ctx.JSON(http.StatusOK, r)
}
