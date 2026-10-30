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
		a.op("listCheckpoints", false),
		forge.WithRequestSchema(ListCheckpointsRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Checkpoint list", ListCheckpointsResponse{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register checkpoint routes: %w", err)
	}

	if err := g.POST("/checkpoints/:id/resolve", a.resolveCheckpoint,
		forge.WithSummary("Resolve checkpoint"),
		forge.WithDescription("Approves or rejects a pending checkpoint."),
		a.op("resolveCheckpoint", true),
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
	resp := newListCheckpointsResponse(cps)
	return resp, nil
}

// newListCheckpointsResponse serves checkpoints without the decider's
// principal bytes, which are the host's and stay in the store.
func newListCheckpointsResponse(cps []*checkpoint.Checkpoint) *ListCheckpointsResponse {
	items := make([]*CheckpointResponse, 0, len(cps))
	for _, cp := range cps {
		items = append(items, newCheckpointResponse(cp))
	}
	return &ListCheckpointsResponse{Items: items}
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

// decisionFromRequest turns the wire decision into the one the engine
// acts on.
//
// It is a function rather than a switch inline in the handler so the
// mapping itself can be tested. Which string means approval is now a
// live decision about a live run: "approved" resumes it and dispatches
// the call a human just granted, "rejected" fails it outright. Inverting
// the two would pass every test written against the handler's error
// paths, and the first thing to notice would be a production run that
// failed on an approval.
func decisionFromRequest(req *ResolveCheckpointRequest) (checkpoint.Decision, error) {
	var approved bool
	switch req.Decision {
	case approvedDecision:
		approved = true
	case rejectedDecision:
		approved = false
	default:
		return checkpoint.Decision{}, forge.BadRequest(fmt.Sprintf(
			"invalid decision %q: must be %q or %q", req.Decision, approvedDecision, rejectedDecision))
	}
	return checkpoint.Decision{
		Approved:  approved,
		DecidedBy: req.DecidedBy,
		Reason:    req.Reason,
	}, nil
}

func (a *API) resolveCheckpoint(ctx forge.Context, req *ResolveCheckpointRequest) (*struct{}, error) {
	cpID, err := id.ParseCheckpointID(req.CheckpointID)
	if err != nil {
		return nil, forge.BadRequest(fmt.Sprintf("invalid checkpoint ID: %v", err))
	}

	decision, err := decisionFromRequest(req)
	if err != nil {
		return nil, err
	}
	// With an authorizer the decider is whoever the request principal
	// is, never the name the body asserts. The engine records that same
	// principal as DecidedByPrincipal from the context.
	decision.DecidedBy, err = a.decidedBy(ctx.Context(), req.DecidedBy)
	if err != nil {
		return nil, err
	}

	if err := a.eng.ResolveCheckpoint(ctx.Context(), cpID, decision); err != nil {
		return nil, mapStoreError(err)
	}

	return nil, ctx.NoContent(http.StatusNoContent)
}
