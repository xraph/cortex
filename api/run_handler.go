package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/xraph/forge"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

func (a *API) registerRunRoutes(router forge.Router) error {
	g := router.Group("/v1", forge.WithGroupTags("runs"))

	if err := g.GET("/runs", a.listRuns,
		forge.WithSummary("List runs"),
		forge.WithDescription("Returns agent runs with optional pagination."),
		a.op("listRuns", false),
		forge.WithRequestSchema(ListRunsRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Run list", []*run.Run{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register run routes: %w", err)
	}

	if err := g.GET("/runs/:id", a.getRun,
		forge.WithSummary("Get run"),
		forge.WithDescription("Returns details of a specific run."),
		a.op("getRun", false),
		forge.WithResponseSchema(http.StatusOK, "Run details", &run.Run{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register run routes: %w", err)
	}

	if err := g.GET("/runs/:id/results/:result_id", a.readResultContent,
		forge.WithSummary("Read screened verified result content"),
		forge.WithDescription("Checks current content authority, retained verification and publication policy before bounded delivery."),
		a.op("readResultContent", false),
		forge.WithRequestSchema(ResultContentRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Screened result", verification.ResultContentResponse{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register result content: %w", err)
	}

	if err := g.GET("/runs/:id/events", a.replayRun,
		forge.WithSummary("Replay public run metadata"),
		forge.WithDescription("Reads authorized durable events. A 410 page identifies a retention gap and requires a refreshed run snapshot."),
		a.op("replayRunEvents", false),
		forge.WithRequestSchema(ReplayRunRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Public event page", event.ReplayPage{}),
		forge.WithResponseSchema(http.StatusGone, "Retention gap", event.ReplayPage{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register run replay: %w", err)
	}

	if err := g.GET("/runs/:id/events/stream", a.streamReplayRun,
		forge.WithSummary("Subscribe to public run metadata"),
		forge.WithDescription("Bounded SSE replay with run-bound event IDs, current subscriber authorization and explicit retention gaps."),
		a.op("streamRunEvents", false),
		forge.WithResponseContentTypes("text/event-stream"),
		forge.WithSSEMessages(map[string]any{"page": event.ReplayPage{}, "heartbeat": event.ReplayPage{}, "gap": event.ReplayPage{}, "error": event.ReplayFailure{}}),
		forge.WithRequestSchema(ReplayStreamRequest{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register run replay stream: %w", err)
	}

	if err := g.POST("/runs/:id/cancel", a.cancelRun,
		forge.WithSummary("Cancel run"),
		forge.WithDescription("Cancels a running agent execution."),
		a.op("cancelRun", true),
		forge.WithNoContentResponse(),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register run routes: %w", err)
	}

	return nil
}

func (a *API) getRun(ctx forge.Context, _ *GetRunRequest) (*run.Run, error) {
	ctx.SetHeader("Cache-Control", "no-store")
	runID, err := id.ParseAgentRunID(ctx.Param("id"))
	if err != nil {
		return nil, forge.BadRequest(fmt.Sprintf("invalid run ID: %v", err))
	}

	r, err := a.eng.GetRun(ctx.Context(), runID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	r = run.PublicView(r, a.eng.ResultContentAvailable())
	return r, ctx.JSON(http.StatusOK, r)
}

func (a *API) listRuns(ctx forge.Context, req *ListRunsRequest) (*ListRunsResponse, error) {
	ctx.SetHeader("Cache-Control", "no-store")
	runs, err := a.eng.ListRuns(ctx.Context(), &run.ListFilter{
		Limit:  defaultLimit(req.Limit),
		Offset: req.Offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	publicRuns := make([]*run.Run, len(runs))
	for i, current := range runs {
		publicRuns[i] = run.PublicView(current, a.eng.ResultContentAvailable())
	}
	resp := &ListRunsResponse{Items: publicRuns}
	return resp, ctx.JSON(http.StatusOK, resp)
}

func (a *API) cancelRun(ctx forge.Context, _ *CancelRunRequest) (*struct{}, error) {
	runID, err := id.ParseAgentRunID(ctx.Param("id"))
	if err != nil {
		return nil, forge.BadRequest(fmt.Sprintf("invalid run ID: %v", err))
	}

	// The engine cancels, rather than this handler reading the run and
	// writing it back: a paused run has to be claimed before it is
	// written, and the claim is engine machinery.
	if err := a.eng.CancelRun(ctx.Context(), runID); err != nil {
		if errors.Is(err, cortex.ErrInvalidState) {
			return nil, forge.BadRequest(err.Error())
		}
		if errors.Is(err, cortex.ErrNotSuspended) {
			// The run was paused when it was read and somebody else had
			// claimed it by the time the cancel tried to. A retry is the
			// right answer, and the run's new state says whether one is
			// still wanted, so this is a conflict rather than a failure.
			return nil, forge.NewHTTPError(http.StatusConflict, err.Error())
		}
		return nil, mapStoreError(err)
	}

	return nil, ctx.NoContent(http.StatusNoContent)
}
