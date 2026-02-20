package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/forge"
)

func (a *API) registerRunRoutes(router forge.Router) {
	g := router.Group("/cortex", forge.WithGroupTags("runs"))

	_ = g.GET("/runs", a.listRuns,
		forge.WithSummary("List runs"),
		forge.WithDescription("Returns agent runs with optional pagination."),
		forge.WithOperationID("listRuns"),
		forge.WithRequestSchema(ListRunsRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Run list", []*run.Run{}),
		forge.WithErrorResponses(),
	)

	_ = g.GET("/runs/:id", a.getRun,
		forge.WithSummary("Get run"),
		forge.WithDescription("Returns details of a specific run."),
		forge.WithOperationID("getRun"),
		forge.WithResponseSchema(http.StatusOK, "Run details", &run.Run{}),
		forge.WithErrorResponses(),
	)

	_ = g.POST("/runs/:id/cancel", a.cancelRun,
		forge.WithSummary("Cancel run"),
		forge.WithDescription("Cancels a running agent execution."),
		forge.WithOperationID("cancelRun"),
		forge.WithNoContentResponse(),
		forge.WithErrorResponses(),
	)
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

func (a *API) listRuns(ctx forge.Context, req *ListRunsRequest) ([]*run.Run, error) {
	runs, err := a.eng.ListRuns(ctx.Context(), &run.ListFilter{
		Limit:  defaultLimit(req.Limit),
		Offset: req.Offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	return runs, ctx.JSON(http.StatusOK, runs)
}

func (a *API) cancelRun(_ forge.Context, _ *CancelRunRequest) (*struct{}, error) {
	// TODO: implement run cancellation in phase 2
	return nil, forge.NotFound("run cancellation not yet implemented")
}
