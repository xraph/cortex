package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/forge"

	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
)

func (a *API) registerCheckpointRoutes(router forge.Router) error {
	g := router.Group("/v1", forge.WithGroupTags("checkpoints"))

	if err := g.GET("/checkpoints", a.listCheckpoints,
		forge.WithSummary("List pending checkpoints"),
		forge.WithDescription("Returns checkpoints awaiting human decision."),
		forge.WithOperationID("listCheckpoints"),
		forge.WithRequestSchema(ListCheckpointsRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Checkpoint list", []*checkpoint.Checkpoint{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register checkpoint routes: %w", err)
	}

	if err := g.POST("/checkpoints/:id/resolve", a.resolveCheckpoint,
		forge.WithSummary("Resolve checkpoint"),
		forge.WithDescription("Approves or rejects a pending checkpoint."),
		forge.WithOperationID("resolveCheckpoint"),
		forge.WithRequestSchema(ResolveCheckpointRequest{}),
		forge.WithNoContentResponse(),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register checkpoint routes: %w", err)
	}

	return nil
}

func (a *API) listCheckpoints(ctx forge.Context, req *ListCheckpointsRequest) (*ListCheckpointsResponse, error) {
	cps, err := a.eng.ListPendingCheckpoints(ctx.Context(), &checkpoint.ListFilter{
		Limit:  defaultLimit(req.Limit),
		Offset: req.Offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list checkpoints: %w", err)
	}
	resp := &ListCheckpointsResponse{Items: cps}
	return resp, ctx.JSON(http.StatusOK, resp)
}

// Decision values the API accepts on the wire. These are the only two
// strings ResolveCheckpointRequest.Decision documents, and resolveCheckpoint
// rejects anything else outright rather than folding an unrecognised value
// into approvedDecision, which used to read a typo as a rejection and fail
// a live run instead of reporting the bad request.
const (
	approvedDecision = "approved"
	rejectedDecision = "rejected"
)

func (a *API) resolveCheckpoint(ctx forge.Context, req *ResolveCheckpointRequest) (*struct{}, error) {
	cpID, err := id.ParseCheckpointID(req.CheckpointID)
	if err != nil {
		return nil, forge.BadRequest(fmt.Sprintf("invalid checkpoint ID: %v", err))
	}

	var approved bool
	switch req.Decision {
	case approvedDecision:
		approved = true
	case rejectedDecision:
		approved = false
	default:
		return nil, forge.BadRequest(fmt.Sprintf(
			"invalid decision %q: must be %q or %q", req.Decision, approvedDecision, rejectedDecision))
	}

	decision := checkpoint.Decision{
		Approved:  approved,
		DecidedBy: req.DecidedBy,
		Reason:    req.Reason,
	}

	if err := a.eng.ResolveCheckpoint(ctx.Context(), cpID, decision); err != nil {
		return nil, mapStoreError(err)
	}

	return nil, ctx.NoContent(http.StatusNoContent)
}
