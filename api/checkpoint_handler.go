package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/forge"
)

func (a *API) registerCheckpointRoutes(router forge.Router) {
	g := router.Group("/cortex", forge.WithGroupTags("checkpoints"))

	_ = g.GET("/checkpoints", a.listCheckpoints,
		forge.WithSummary("List pending checkpoints"),
		forge.WithDescription("Returns checkpoints awaiting human decision."),
		forge.WithOperationID("listCheckpoints"),
		forge.WithRequestSchema(ListCheckpointsRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Checkpoint list", []*checkpoint.Checkpoint{}),
		forge.WithErrorResponses(),
	)

	_ = g.POST("/checkpoints/:id/resolve", a.resolveCheckpoint,
		forge.WithSummary("Resolve checkpoint"),
		forge.WithDescription("Approves or rejects a pending checkpoint."),
		forge.WithOperationID("resolveCheckpoint"),
		forge.WithRequestSchema(ResolveCheckpointRequest{}),
		forge.WithNoContentResponse(),
		forge.WithErrorResponses(),
	)
}

func (a *API) listCheckpoints(ctx forge.Context, req *ListCheckpointsRequest) ([]*checkpoint.Checkpoint, error) {
	cps, err := a.eng.ListPendingCheckpoints(ctx.Context(), &checkpoint.ListFilter{
		Limit:  defaultLimit(req.Limit),
		Offset: req.Offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list checkpoints: %w", err)
	}
	return cps, ctx.JSON(http.StatusOK, cps)
}

func (a *API) resolveCheckpoint(ctx forge.Context, req *ResolveCheckpointRequest) (*struct{}, error) {
	cpID, err := id.ParseCheckpointID(req.CheckpointID)
	if err != nil {
		return nil, forge.BadRequest(fmt.Sprintf("invalid checkpoint ID: %v", err))
	}

	decision := checkpoint.Decision{
		Approved:  req.Decision == "approved",
		DecidedBy: req.DecidedBy,
	}

	if err := a.eng.ResolveCheckpoint(ctx.Context(), cpID, decision); err != nil {
		return nil, mapStoreError(err)
	}

	return nil, ctx.NoContent(http.StatusNoContent)
}
