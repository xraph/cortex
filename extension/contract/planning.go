package contract

import (
	"context"
	"errors"

	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/verification"
)

type planningListInput struct {
	After string `json:"after"`
	Limit int    `json:"limit"`
}
type planningListOutput struct {
	Available    bool                              `json:"available"`
	Reason       string                            `json:"reason,omitempty"`
	Items        []coordination.PlanningPlanView   `json:"items"`
	Capabilities coordination.PlanningCapabilities `json:"capabilities"`
}

func planningError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, coordination.ErrPlanningDenied), errors.Is(err, evidence.ErrDenied), errors.Is(err, verification.ErrResultAccessDenied):
		return denied("Current team operation permission is required")
	case errors.Is(err, coordination.ErrPlanningUnavailable):
		return &dc.Error{Code: dc.CodeUnavailable, Message: "This team operation is not installed"}
	case errors.Is(err, planning.ErrInvalid), errors.Is(err, coordination.ErrInvalidClaim):
		return bad("Invalid team operation")
	case errors.Is(err, planning.ErrConflict), errors.Is(err, coordination.ErrClaimConflict), errors.Is(err, coordination.ErrAssignmentConflict):
		return &dc.Error{Code: dc.CodeConflict, Message: "The plan or task changed. Reload before retrying."}
	case errors.Is(err, planning.ErrNotFound), errors.Is(err, coordination.ErrClaimNotFound):
		return &dc.Error{Code: dc.CodeNotFound, Message: "The plan or task is unavailable in your scope"}
	default:
		return mapError(err)
	}
}

func (s *service) bindPlanning(d *dispatcher.Dispatcher) error {
	if err := query(d, s, "planning.list", func(ctx context.Context, in planningListInput) (planningListOutput, error) {
		if s.deps.Planning == nil {
			return planningListOutput{Reason: "Authorized team planning is not installed", Items: []coordination.PlanningPlanView{}}, nil
		}
		request := planning.ListRequest{Limit: in.Limit}
		if request.Limit == 0 {
			request.Limit = 25
		}
		var err error
		if in.After != "" {
			request.After, err = id.ParseWithPrefix(in.After, planning.PrefixPlan)
			if err != nil {
				return planningListOutput{}, bad("Invalid plan cursor")
			}
		}
		items, err := s.deps.Planning.List(ctx, request)
		return planningListOutput{Available: err == nil, Items: items, Capabilities: s.deps.Planning.Capabilities()}, planningError(err)
	}); err != nil {
		return err
	}
	if err := query(d, s, "planning.detail", func(ctx context.Context, in IDInput) (coordination.PlanningDetail, error) {
		key, err := id.ParseWithPrefix(in.ID, planning.PrefixPlan)
		if err != nil {
			return coordination.PlanningDetail{}, bad("Invalid plan ID")
		}
		out, err := s.deps.Planning.Detail(ctx, key)
		return out, planningError(err)
	}); err != nil {
		return err
	}
	return command(d, s, "planning.command", "manage", func(ctx context.Context, in coordination.PlanningCommand) (coordination.PlanningCommandResult, error) {
		out, err := s.deps.Planning.Command(ctx, in)
		return out, planningError(err)
	})
}
