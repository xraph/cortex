package coordination

import (
	"context"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/run"
)

// PlanningCancellationStore retains the selected assignment and producer run.
type PlanningCancellationStore interface {
	PlanningOperatorStore
	GetRun(context.Context, id.AgentRunID) (*run.Run, error)
}

// PlanningCancellationAuthority authenticates the actor and excludes delayed
// dispatch, task acceptance and grant changes throughout cancellation. The fence
// must be shared by those writers across host processes. Actor is host-resolved.
type PlanningCancellationAuthority interface {
	WithPlanningCancellationFence(context.Context, PlanningCommand, func() error) error
	AuthorizePlanningCancellation(context.Context, PlanningCommand, Assignment) (string, error)
}

// PlanningRunCanceller supplies the existing managed run cancellation operation.
// Its return does not attest external effects have stopped or justify refunds.
type PlanningRunCanceller interface {
	CancelRun(context.Context, id.AgentRunID) error
}

type planningCanceller struct {
	store     PlanningCancellationStore
	authority PlanningCancellationAuthority
	runtime   PlanningRunCanceller
}

// NewPlanningCancellation constructs guarded assignment cancellation. It keeps
// task ownership and shared charges until normal proof-based recovery finishes.
func NewPlanningCancellation(store PlanningCancellationStore, authority PlanningCancellationAuthority, runtime PlanningRunCanceller) (PlanningCancellation, error) {
	if hostport.IsNil(store) || hostport.IsNil(authority) || hostport.IsNil(runtime) {
		return nil, planning.ErrInvalid
	}
	return &planningCanceller{store, authority, runtime}, nil
}

func (c *planningCanceller) CancelPlanningAssignment(ctx context.Context, command PlanningCommand) (Assignment, error) {
	if command.Validate() != nil || command.Operation != "cancel" {
		return Assignment{}, planning.ErrInvalid
	}
	return hostport.WithSynchronousFence(ctx, planning.ErrConflict, func(attempt context.Context, next func(struct{}) error) error {
		return c.authority.WithPlanningCancellationFence(attempt, command, func() error { return next(struct{}{}) })
	}, func(attempt context.Context, _ struct{}) (Assignment, error) {
		plan, err := c.store.GetPlan(attempt, command.PlanID)
		if err != nil {
			return Assignment{}, err
		}
		if planning.Validate(plan, planning.Limits{}) != nil || plan.Revision != command.ExpectedRevision || plan.Digest != command.ExpectedDigest || plan.Scope.Canonical() != cortex.ScopeFromContext(attempt).Canonical() {
			return Assignment{}, planning.ErrConflict
		}
		claim, err := c.store.GetTaskClaim(attempt, command.PlanID, command.TaskID)
		if err != nil {
			return Assignment{}, err
		}
		if claim.Validate() != nil || claim.Revision != command.ExpectedClaimRevision || claim.PlanDigest != plan.Digest || claim.Scope.Canonical() != plan.Scope.Canonical() || claim.State == ClaimCompleted {
			return Assignment{}, planning.ErrConflict
		}
		dispatch, err := c.store.GetTaskDispatch(attempt, claim.ID)
		if err != nil {
			return Assignment{}, err
		}
		if dispatch.Assignment.Handoff.AssignmentID != command.OperationID {
			return Assignment{}, planning.ErrConflict
		}
		current, err := c.store.GetAssignment(attempt, command.OperationID)
		if err != nil {
			return Assignment{}, err
		}
		h := current.Handoff
		if current.Validate() != nil || h.PlanID != plan.ID || h.PlanDigest != plan.Digest || h.TaskID != command.TaskID || h.Fence != claim.Fence || h.Scope.Canonical() != plan.Scope.Canonical() {
			return Assignment{}, planning.ErrConflict
		}
		actor, err := c.authority.AuthorizePlanningCancellation(attempt, command, current)
		if err != nil {
			return Assignment{}, errors.Join(ErrPlanningDenied, err)
		}
		change := AssignmentChange{AssignmentID: command.OperationID, ExpectedRevision: command.ExpectedAssignmentRevision, HandoffDigest: h.Digest, Fence: h.Fence, State: AssignmentCancelled, Actor: actor, Reason: command.Reason}
		_, digest, err := change.Freeze()
		if err != nil {
			return Assignment{}, err
		}
		if current.State == AssignmentCancelled && current.Revision == command.ExpectedAssignmentRevision+1 && current.RequestDigest == digest {
			return current, nil
		}
		if current.Revision != command.ExpectedAssignmentRevision {
			return Assignment{}, planning.ErrConflict
		}
		// Validate the transition before asking the runtime to change its state.
		if _, err = Advance(current, change, current.At); err != nil {
			return Assignment{}, err
		}
		producer, err := c.store.GetRun(attempt, claim.RunID)
		if err != nil && !errors.Is(err, cortex.ErrRunNotFound) {
			return Assignment{}, err
		}
		if err == nil {
			if producer == nil || producer.ID != claim.RunID || producer.Scope.Canonical() != plan.Scope.Canonical() {
				return Assignment{}, planning.ErrConflict
			}
			if producer.State == run.StateRunning || producer.State == run.StatePaused {
				if cancelErr := c.runtime.CancelRun(attempt, claim.RunID); cancelErr != nil {
					return Assignment{}, cancelErr
				}
			}
		}
		cancelled, err := c.store.AdvanceAssignment(attempt, change)
		if err != nil {
			return Assignment{}, err
		}
		if _, err = c.authority.AuthorizePlanningCancellation(attempt, command, cancelled); err != nil {
			return Assignment{}, errors.Join(ErrPlanningDenied, err)
		}
		return cancelled, nil
	})
}
