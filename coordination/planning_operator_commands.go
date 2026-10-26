package coordination

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/planning"
)

// PlanningCommand carries selection and optimistic pins, never authority inputs.
// OperationID is a retained request identity and must be reused after lost replies.
type PlanningCommand struct {
	Operation                  string `json:"operation"`
	OperationID                id.ID  `json:"operation_id"`
	PlanID                     id.ID  `json:"plan_id"`
	ExpectedRevision           int64  `json:"expected_revision"`
	ExpectedDigest             string `json:"expected_digest"`
	TaskID                     id.ID  `json:"task_id,omitempty"`
	ExpectedClaimRevision      int64  `json:"expected_claim_revision,omitempty"`
	ExpectedAssignmentRevision int64  `json:"expected_assignment_revision,omitempty"`
	Worker                     string `json:"worker,omitempty"`
	ExpectedResourceRevision   int64  `json:"expected_resource_revision,omitempty"`
	ExpectedResourceDigest     string `json:"expected_resource_digest,omitempty"`
	ConflictID                 id.ID  `json:"conflict_id,omitempty"`
	Reason                     string `json:"reason"`
}

// Validate bounds transport input before any trusted resolver is called.
func (c PlanningCommand) Validate() error {
	if c.OperationID.IsNil() || c.PlanID.IsNil() || c.PlanID.Prefix() != planning.PrefixPlan || c.ExpectedRevision < 1 || !evidence.ValidDigest(c.ExpectedDigest) || !handoffText(c.Reason, 4096) || c.ExpectedClaimRevision < 0 || c.ExpectedAssignmentRevision < 0 || len(c.Worker) > 512 {
		return planning.ErrInvalid
	}
	switch c.Operation {
	case "claim", "retry", "reassign":
		if c.OperationID.Prefix() != PrefixClaim || c.TaskID.IsNil() || c.TaskID.Prefix() != planning.PrefixTask {
			return planning.ErrInvalid
		}
	case "propose_revision", "observe_revision", "accept_revision":
		if c.OperationID.Prefix() != PrefixPlanReplan || !c.TaskID.IsNil() {
			return planning.ErrInvalid
		}
	case "reconcile", "cancel":
		if c.OperationID.Prefix() != PrefixAssignment || c.TaskID.IsNil() || c.TaskID.Prefix() != planning.PrefixTask || c.ExpectedAssignmentRevision < 1 {
			return planning.ErrInvalid
		}
	case "resolve_conflict":
		if c.OperationID.Prefix() != PrefixBoardTransition || c.ConflictID.IsNil() || c.ConflictID.Prefix() != PrefixBoardTransition || c.ExpectedResourceRevision < 0 || c.ExpectedResourceRevision > MaxBoardTransitions || (c.ExpectedResourceRevision == 0 && c.ExpectedResourceDigest != "") || (c.ExpectedResourceRevision > 0 && !evidence.ValidDigest(c.ExpectedResourceDigest)) {
			return planning.ErrInvalid
		}
	default:
		return planning.ErrInvalid
	}
	return nil
}

// PlanningClaimResolver authenticates worker selection and persists stable
// request identities. It must not infer new grants from an operator command.
type PlanningClaimResolver interface {
	ResolvePlanningClaim(context.Context, PlanningCommand) (ReadyTaskClaimRequest, error)
}

// PlanningReplanResolver reconstructs trusted progress, runtime pins and proofs.
type PlanningReplanResolver interface {
	ResolvePlanningReplan(context.Context, PlanningCommand) (PlanReplanControl, error)
}

// PlanningArtifactResolver resolves current grants and verified artifact bindings.
type PlanningArtifactResolver interface {
	ResolvePlanningArtifact(context.Context, PlanningCommand) (ArtifactBoardRequest, error)
}

// PlanningCancellation stops a selected assignment through a current authority
// fence. Cancellation never releases a reservation or proves effects quiescent.
type PlanningCancellation interface {
	CancelPlanningAssignment(context.Context, PlanningCommand) (Assignment, error)
}

// PlanningCommandResult is an acknowledgment, not evidence of task success.
type PlanningCommandResult struct {
	OperationID  id.ID  `json:"operation_id"`
	Operation    string `json:"operation"`
	Fresh        bool   `json:"fresh"`
	PlanRevision int64  `json:"plan_revision,omitempty"`
	PlanDigest   string `json:"plan_digest,omitempty"`
	ClaimID      id.ID  `json:"claim_id,omitempty"`
	AssignmentID id.ID  `json:"assignment_id,omitempty"`
	Pending      bool   `json:"pending"`
}

// Command routes only to installed guarded services. A failed acknowledgment
// must be reconciled with the same operation identity, never a fresh dispatch.
func (o *PlanningOperator) Command(ctx context.Context, c PlanningCommand) (PlanningCommandResult, error) {
	if o == nil {
		return PlanningCommandResult{}, ErrPlanningUnavailable
	}
	if err := c.Validate(); err != nil {
		return PlanningCommandResult{}, err
	}
	if cortex.ValidateScope(cortex.ScopeFromContext(ctx)) != nil {
		return PlanningCommandResult{}, cortex.ErrNoScope
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	access := PlanningAccess{PlanID: c.PlanID, Operation: c.Operation}
	if err := o.config.Authority.AuthorizePlanning(ctx, access); err != nil {
		return PlanningCommandResult{}, errors.Join(ErrPlanningDenied, err)
	}
	p, err := o.config.Store.GetPlan(ctx, c.PlanID)
	if err != nil {
		return PlanningCommandResult{}, err
	}
	if planning.Validate(p, planning.Limits{}) != nil || p.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return PlanningCommandResult{}, planning.ErrConflict
	}
	// Commit authenticates exact historical retries against its retained control.
	// Other operations still require the selected graph to be current.
	if c.Operation != "accept_revision" && (p.Revision != c.ExpectedRevision || p.Digest != c.ExpectedDigest) {
		return PlanningCommandResult{}, planning.ErrConflict
	}
	result := PlanningCommandResult{OperationID: c.OperationID, Operation: c.Operation, PlanRevision: p.Revision, PlanDigest: p.Digest}
	switch c.Operation {
	case "claim", "retry", "reassign":
		if (c.Operation == "retry" && !o.config.Retry) || (c.Operation == "reassign" && !o.config.Reassign) {
			return result, ErrPlanningUnavailable
		}
		result, err = o.commandClaim(ctx, c, result)
	case "propose_revision", "observe_revision", "accept_revision":
		result, err = o.commandReplan(ctx, c, result)
	case "reconcile":
		if o.config.Completion == nil {
			return result, ErrPlanningUnavailable
		}
		var current Assignment
		current, err = o.commandAssignment(ctx, c)
		if err == nil {
			err = completeAssignment(ctx, *o.config.Completion, current.Handoff.AssignmentID)
			result.AssignmentID = current.Handoff.AssignmentID
			result.Pending = true
		}
	case "cancel":
		if hostport.IsNil(o.config.Cancellation) {
			return result, ErrPlanningUnavailable
		}
		var assignment Assignment
		assignment, err = o.config.Cancellation.CancelPlanningAssignment(ctx, c)
		if err == nil && (assignment.Validate() != nil || assignment.State != AssignmentCancelled || assignment.Handoff.AssignmentID != c.OperationID || assignment.Handoff.PlanID != c.PlanID || assignment.Handoff.PlanDigest != c.ExpectedDigest || assignment.Handoff.TaskID != c.TaskID || assignment.Handoff.Scope.Canonical() != p.Scope.Canonical()) {
			err = planning.ErrConflict
		}
		result.AssignmentID = assignment.Handoff.AssignmentID
	case "resolve_conflict":
		if o.config.Artifacts == nil {
			return result, ErrPlanningUnavailable
		}
		var request ArtifactBoardRequest
		request, err = o.config.ArtifactResolver.ResolvePlanningArtifact(ctx, c)
		if err == nil && (request.ID != c.OperationID || request.PlanID != c.PlanID || request.PlanDigest != c.ExpectedDigest || request.Scope.Canonical() != p.Scope.Canonical() || request.Resolve != c.ConflictID || request.ExpectedRevision != c.ExpectedResourceRevision || request.ExpectedDigest != c.ExpectedResourceDigest) {
			err = planning.ErrConflict
		}
		if err == nil {
			_, err = o.config.Artifacts.Write(ctx, request)
		}
	}
	if err != nil {
		return result, err
	}
	if err = o.config.Authority.AuthorizePlanning(ctx, access); err != nil {
		return result, errors.Join(ErrPlanningDenied, err)
	}
	return result, ctx.Err()
}

func (o *PlanningOperator) commandClaim(ctx context.Context, c PlanningCommand, result PlanningCommandResult) (PlanningCommandResult, error) {
	if hostport.IsNil(o.config.Claims) {
		return result, ErrPlanningUnavailable
	}
	req, err := o.config.Claims.ResolvePlanningClaim(ctx, c)
	if err != nil {
		return result, err
	}
	frozen, _, err := req.Freeze()
	if err != nil {
		return result, err
	}
	claim := frozen.Acquisition.Claim
	if claim.ID != c.OperationID || claim.PlanID != c.PlanID || claim.PlanRevision != c.ExpectedRevision || claim.PlanDigest != c.ExpectedDigest || claim.TaskID != c.TaskID || claim.ExpectedRevision != c.ExpectedClaimRevision || (c.Worker != "" && claim.Worker.Agent != c.Worker) || frozen.Acquisition.Reservation.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return result, planning.ErrConflict
	}
	admitted, err := o.config.Admitter.Admit(ctx, frozen)
	if err != nil {
		return result, err
	}
	if admitted.Validate(frozen) != nil {
		return result, planning.ErrConflict
	}
	result.ClaimID, result.Fresh = claim.ID, admitted.Fresh
	if admitted.Fresh {
		err = o.config.Worker.RunClaimedTask(ctx, frozen, admitted)
	}
	return result, err
}

func (o *PlanningOperator) commandReplan(ctx context.Context, c PlanningCommand, result PlanningCommandResult) (PlanningCommandResult, error) {
	if o.config.Replanner == nil {
		return result, ErrPlanningUnavailable
	}
	control, err := o.config.Replans.ResolvePlanningReplan(ctx, c)
	if err != nil {
		return result, err
	}
	control, _, err = control.Freeze()
	if err != nil {
		return result, err
	}
	plan := control.Request.Progress.Plan
	operations := map[string]string{"propose_revision": "dispatch", "observe_revision": "observe", "accept_revision": "commit"}
	if control.Operation != operations[c.Operation] || control.Request.ID != c.OperationID || plan.ID != c.PlanID || plan.Revision != c.ExpectedRevision || plan.Digest != c.ExpectedDigest || plan.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return result, planning.ErrConflict
	}
	switch control.Operation {
	case "dispatch":
		var admitted PlanReplanAdmission
		admitted, err = o.config.Replanner.Execute(ctx, control.Request)
		result.Fresh = admitted.Fresh
		result.Pending = true
	case "observe":
		var observed *PlanReplanOutcome
		observed, err = o.config.Replanner.Observe(ctx, control)
		result.Pending = observed == nil
	case "commit":
		var next planning.Plan
		next, err = o.config.Replanner.Commit(ctx, control)
		if err == nil {
			result.PlanRevision, result.PlanDigest = next.Revision, next.Digest
		}
	}
	return result, err
}

func (o *PlanningOperator) commandAssignment(ctx context.Context, c PlanningCommand) (Assignment, error) {
	claim, err := o.config.Store.GetTaskClaim(ctx, c.PlanID, c.TaskID)
	if err != nil {
		return Assignment{}, err
	}
	if claim.Validate() != nil || claim.PlanDigest != c.ExpectedDigest || claim.Revision != c.ExpectedClaimRevision || claim.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return Assignment{}, planning.ErrConflict
	}
	dispatch, err := o.config.Store.GetTaskDispatch(ctx, claim.ID)
	if err != nil {
		return Assignment{}, err
	}
	if dispatch.Assignment.Handoff.AssignmentID != c.OperationID {
		return Assignment{}, planning.ErrConflict
	}
	assignment, err := o.config.Store.GetAssignment(ctx, c.OperationID)
	if err != nil {
		return Assignment{}, err
	}
	if assignment.Validate() != nil || assignment.Revision != c.ExpectedAssignmentRevision || assignment.Handoff.PlanID != c.PlanID || assignment.Handoff.PlanDigest != c.ExpectedDigest || assignment.Handoff.TaskID != c.TaskID || assignment.Handoff.Fence != claim.Fence || assignment.Handoff.Scope.Canonical() != claim.Scope.Canonical() {
		return Assignment{}, planning.ErrConflict
	}
	return assignment, nil
}
