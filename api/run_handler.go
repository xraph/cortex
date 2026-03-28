package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/forge"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

func (a *API) registerRunRoutes(router forge.Router) error {
	g := router.Group("/v1", forge.WithGroupTags("runs"))

	if err := g.GET("/runs", a.listRuns,
		forge.WithSummary("List runs"),
		forge.WithDescription("Returns agent runs with optional pagination."),
		forge.WithOperationID("listRuns"),
		forge.WithRequestSchema(ListRunsRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Run list", []*run.Run{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register run routes: %w", err)
	}

	if err := g.GET("/runs/:id", a.getRun,
		forge.WithSummary("Get run"),
		forge.WithDescription("Returns details of a specific run."),
		forge.WithOperationID("getRun"),
		forge.WithResponseSchema(http.StatusOK, "Run details", &run.Run{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register run routes: %w", err)
	}

	if err := g.POST("/runs/:id/cancel", a.cancelRun,
		forge.WithSummary("Cancel run"),
		forge.WithDescription("Cancels a running agent execution."),
		forge.WithOperationID("cancelRun"),
		forge.WithNoContentResponse(),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register run routes: %w", err)
	}

	return nil
}

func (a *API) getRun(ctx forge.Context, _ *GetRunRequest) (*run.Run, error) {
	runID, err := id.ParseAgentRunID(ctx.Param("id"))
	if err != nil {
		return nil, forge.BadRequest(fmt.Sprintf("invalid run ID: %v", err))
	}

	r, err := a.eng.GetRun(ctx.Context(), runID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return r, ctx.JSON(http.StatusOK, r)
}

func (a *API) listRuns(ctx forge.Context, req *ListRunsRequest) (*ListRunsResponse, error) {
	runs, err := a.eng.ListRuns(ctx.Context(), &run.ListFilter{
		Limit:  defaultLimit(req.Limit),
		Offset: req.Offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	resp := &ListRunsResponse{Items: runs}
	return resp, ctx.JSON(http.StatusOK, resp)
}

func (a *API) cancelRun(ctx forge.Context, _ *CancelRunRequest) (*struct{}, error) {
	runID, err := id.ParseAgentRunID(ctx.Param("id"))
	if err != nil {
		return nil, forge.BadRequest(fmt.Sprintf("invalid run ID: %v", err))
	}

	r, err := a.eng.GetRun(ctx.Context(), runID)
	if err != nil {
		return nil, mapStoreError(err)
	}

	if r.State != run.StateRunning && r.State != run.StatePaused {
		return nil, forge.BadRequest("run is not in a cancellable state")
	}

	r.State = run.StateCancelled
	if err := a.eng.UpdateRun(ctx.Context(), r); err != nil {
		return nil, fmt.Errorf("cancel run: %w", err)
	}

	return nil, ctx.NoContent(http.StatusNoContent)
}
