package api

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/xraph/forge"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/verification"
)

// WithPlanning installs the shared current-authority team operator service.
func WithPlanning(operator *coordination.PlanningOperator) Option {
	return func(a *API) { a.planning = operator }
}

// PlanningListRequest selects bounded exact-scope plan metadata.
type PlanningListRequest struct {
	After string `query:"after" optional:"true"`
	Limit int    `query:"limit" optional:"true"`
}

// PlanningDetailRequest selects one immutable graph's current projection.
type PlanningDetailRequest struct {
	ID string `path:"id"`
}

func (a *API) registerPlanningRoutes(router forge.Router) error {
	group := router.Group("/v1/planning", forge.WithGroupTags("planning"))
	if err := group.GET("/plans", a.listPlanning, forge.WithOperationID("listPlanningPlans"), forge.WithRequestSchema(PlanningListRequest{}), forge.WithResponseSchema(http.StatusOK, "Current plan headers", []coordination.PlanningPlanView{}), forge.WithErrorResponses()); err != nil {
		return err
	}
	if err := group.GET("/plans/:id", a.detailPlanning, forge.WithOperationID("readPlanningPlan"), forge.WithRequestSchema(PlanningDetailRequest{}), forge.WithResponseSchema(http.StatusOK, "Current team graph", coordination.PlanningDetail{}), forge.WithErrorResponses()); err != nil {
		return err
	}
	return group.POST("/commands", a.commandPlanning, forge.WithOperationID("commandPlanning"), forge.WithRequestSchema(coordination.PlanningCommand{}), forge.WithResponseSchema(http.StatusOK, "Retained operation acknowledgment", coordination.PlanningCommandResult{}), forge.WithErrorResponses())
}

func (a *API) listPlanning(ctx forge.Context, in *PlanningListRequest) (*[]coordination.PlanningPlanView, error) {
	ctx.SetHeader("Cache-Control", "no-store")
	if a.planning == nil {
		return nil, replyPlanningFailure(ctx, coordination.ErrPlanningUnavailable)
	}
	values, err := url.ParseQuery(ctx.Request().URL.RawQuery)
	if err != nil {
		return nil, replyPlanningFailure(ctx, planning.ErrInvalid)
	}
	for key, v := range values {
		if len(v) != 1 || (key != "after" && key != "limit") {
			return nil, replyPlanningFailure(ctx, planning.ErrInvalid)
		}
	}
	request := planning.ListRequest{Limit: in.Limit}
	if request.Limit == 0 {
		request.Limit = 25
	}
	if in.After != "" {
		request.After, err = id.ParseWithPrefix(in.After, planning.PrefixPlan)
		if err != nil {
			return nil, replyPlanningFailure(ctx, planning.ErrInvalid)
		}
	}
	result, err := a.planning.List(ctx.Context(), request)
	if err != nil {
		return nil, replyPlanningFailure(ctx, err)
	}
	return &result, nil
}
func (a *API) detailPlanning(ctx forge.Context, _ *PlanningDetailRequest) (*coordination.PlanningDetail, error) {
	ctx.SetHeader("Cache-Control", "no-store")
	key, err := id.ParseWithPrefix(ctx.Param("id"), planning.PrefixPlan)
	if err != nil {
		return nil, replyPlanningFailure(ctx, planning.ErrInvalid)
	}
	result, err := a.planning.Detail(ctx.Context(), key)
	if err != nil {
		return nil, replyPlanningFailure(ctx, err)
	}
	return &result, nil
}
func (a *API) commandPlanning(ctx forge.Context, in *coordination.PlanningCommand) (*coordination.PlanningCommandResult, error) {
	ctx.SetHeader("Cache-Control", "no-store")
	keys := ctx.Request().Header.Values("Idempotency-Key")
	if len(keys) != 1 || keys[0] != in.OperationID.String() || in.OperationID.IsNil() {
		return nil, replyPlanningFailure(ctx, planning.ErrInvalid)
	}
	result, err := a.planning.Command(ctx.Context(), *in)
	if err != nil {
		return nil, replyPlanningFailure(ctx, err)
	}
	return &result, nil
}
func replyPlanningFailure(ctx forge.Context, err error) error {
	status, code, message := planningFailure(err)
	return ctx.JSON(status, map[string]string{"code": code, "message": message})
}
func planningFailure(err error) (status int, code, message string) {
	status, code, message = http.StatusServiceUnavailable, "planning_unavailable", "The team operation is unavailable"
	switch {
	case errors.Is(err, coordination.ErrPlanningDenied), errors.Is(err, evidence.ErrDenied), errors.Is(err, verification.ErrResultAccessDenied):
		status, code, message = http.StatusForbidden, "planning_denied", "Current team operation permission is required"
	case errors.Is(err, planning.ErrInvalid), errors.Is(err, coordination.ErrInvalidClaim):
		status, code, message = http.StatusBadRequest, "planning_invalid", "Invalid team operation"
	case errors.Is(err, planning.ErrConflict), errors.Is(err, coordination.ErrClaimConflict), errors.Is(err, coordination.ErrAssignmentConflict):
		status, code, message = http.StatusConflict, "planning_conflict", "The plan or task changed. Reload before retrying."
	case errors.Is(err, planning.ErrNotFound), errors.Is(err, coordination.ErrClaimNotFound):
		status, code, message = http.StatusNotFound, "planning_not_found", "The plan or task is unavailable in your scope"
	}
	return status, code, message
}
