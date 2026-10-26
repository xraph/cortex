package coordination

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/verification"
)

// ErrPlanningDenied distinguishes current operator permission from availability.
var ErrPlanningDenied = errors.New("cortex: planning operator access denied")

// ErrPlanningUnavailable means the host has not installed a guarded capability.
var ErrPlanningUnavailable = errors.New("cortex: planning operator unavailable")

// PlanningAccess identifies metadata access without carrying credentials.
type PlanningAccess struct {
	PlanID    id.ID  `json:"plan_id,omitempty"`
	Operation string `json:"operation"`
}

// PlanningOperatorAuthority authenticates the current caller before and after
// reads. The read fence must hold external grants and source revisions until the
// synchronous callback returns. List permission covers the exact scoped roster.
type PlanningOperatorAuthority interface {
	AuthorizePlanning(context.Context, PlanningAccess) error
	WithPlanningReadFence(context.Context, PlanningAccess, func() error) error
}

// PlanningOperatorStore supplies retained facts; it is not an authorization port.
type PlanningOperatorStore interface {
	planning.Store
	ClaimStore
	TaskDispatchStore
	AssignmentStore
	TaskAcceptanceStore
	CurrentTaskAcceptanceStore
	PlanReplanStore
	execution.BudgetStore
	verification.ResultSnapshotStore
}

// PlanningOperatorConfig joins projections with existing guarded command ports.
// Source reconstructs current bindings. Reports supplied by Source never replace
// authoritative snapshots. Every optional command needs its resolver and runtime.
type PlanningOperatorConfig struct {
	Store     PlanningOperatorStore
	Source    SchedulingSource
	Authority PlanningOperatorAuthority
	Clock     verification.Clock
	// Retry and Reassign require explicit host support in the claim resolver.
	Retry, Reassign  bool
	Claims           PlanningClaimResolver
	Admitter         TaskClaimAdmitter
	Worker           ClaimedTaskWorker
	Replans          PlanningReplanResolver
	Replanner        *PlanReplanner
	Completion       *TaskCompletionServiceConfig
	Artifacts        *ArtifactBoard
	ArtifactResolver PlanningArtifactResolver
	ArtifactReads    PlanningArtifactReadResolver
	Cancellation     PlanningCancellation
}

// PlanningOperator exposes bounded metadata, never candidate or context bytes.
type PlanningOperator struct{ config PlanningOperatorConfig }

// PlanningCapabilities reports installed services, not live qualification.
type PlanningCapabilities struct {
	Claim           bool `json:"claim"`
	Retry           bool `json:"retry"`
	Reassign        bool `json:"reassign"`
	Replan          bool `json:"replan"`
	Reconcile       bool `json:"reconcile"`
	ResolveConflict bool `json:"resolve_conflict"`
	Cancel          bool `json:"cancel"`
}

// NewPlanningOperator rejects incomplete optional service groups.
func NewPlanningOperator(c PlanningOperatorConfig) (*PlanningOperator, error) {
	if hostport.IsNil(c.Store) || hostport.IsNil(c.Source) || hostport.IsNil(c.Authority) || hostport.IsNil(c.Clock) {
		return nil, planning.ErrInvalid
	}
	claim := !hostport.IsNil(c.Claims) || !hostport.IsNil(c.Admitter) || !hostport.IsNil(c.Worker)
	if !claim && (c.Retry || c.Reassign) {
		return nil, planning.ErrInvalid
	}
	if claim && (hostport.IsNil(c.Claims) || hostport.IsNil(c.Admitter) || hostport.IsNil(c.Worker)) {
		return nil, planning.ErrInvalid
	}
	if (c.Replanner == nil) != hostport.IsNil(c.Replans) || (c.Artifacts == nil) != hostport.IsNil(c.ArtifactResolver) {
		return nil, planning.ErrInvalid
	}
	if !hostport.IsNil(c.ArtifactReads) && c.Artifacts == nil {
		return nil, planning.ErrInvalid
	}
	if c.Completion != nil {
		copyConfig := *c.Completion
		if hostport.IsNil(copyConfig.Store) || hostport.IsNil(copyConfig.Resolver) || hostport.IsNil(copyConfig.Authority) || hostport.IsNil(copyConfig.Reviewer) || hostport.IsNil(copyConfig.Acceptor) {
			return nil, planning.ErrInvalid
		}
		c.Completion = &copyConfig
	}
	return &PlanningOperator{config: c}, nil
}

// Capabilities is safe before authorization because it contains no scoped data.
func (o *PlanningOperator) Capabilities() PlanningCapabilities {
	if o == nil {
		return PlanningCapabilities{}
	}
	c := o.config
	return PlanningCapabilities{Claim: !hostport.IsNil(c.Claims), Retry: c.Retry, Reassign: c.Reassign, Replan: c.Replanner != nil, Reconcile: c.Completion != nil, ResolveConflict: c.Artifacts != nil && !hostport.IsNil(c.ArtifactReads), Cancel: !hostport.IsNil(c.Cancellation)}
}

// PlanningPlanView describes one immutable graph revision.
type PlanningPlanView struct {
	ID        id.ID     `json:"id"`
	Revision  int64     `json:"revision"`
	Digest    string    `json:"digest"`
	Objective string    `json:"objective"`
	Tasks     int       `json:"tasks"`
	Deadline  time.Time `json:"deadline"`
}

// PlanningTaskView separates durable acceptance from current verified success.
type PlanningTaskView struct {
	ID                 id.ID            `json:"id"`
	Instruction        string           `json:"instruction"`
	Owner              string           `json:"owner"`
	DependsOn          []id.ID          `json:"depends_on,omitempty"`
	Status             planning.Status  `json:"status"`
	ClaimID            id.ID            `json:"claim_id,omitempty"`
	ClaimRevision      int64            `json:"claim_revision,omitempty"`
	ClaimState         ClaimState       `json:"claim_state,omitempty"`
	RunID              id.AgentRunID    `json:"run_id,omitempty"`
	AssignmentID       id.ID            `json:"assignment_id,omitempty"`
	AssignmentRevision int64            `json:"assignment_revision,omitempty"`
	AssignmentState    AssignmentState  `json:"assignment_state,omitempty"`
	AcceptanceID       id.ID            `json:"acceptance_id,omitempty"`
	AcceptedArtifact   *evidence.Ref    `json:"accepted_artifact,omitempty"`
	CurrentlyAccepted  bool             `json:"currently_accepted"`
	Ready              bool             `json:"ready"`
	ReviewRequired     bool             `json:"review_required"`
	Budget             execution.Budget `json:"budget"`
	NextAction         string           `json:"next_action"`
}

// PlanningDetail is a fenced point-in-time projection, not a dispatch credential.
type PlanningDetail struct {
	Artifacts    []PlanningArtifactView   `json:"artifacts"`
	Replans      []PlanningReplanView     `json:"replans"`
	Plan         PlanningPlanView         `json:"plan"`
	Tasks        []PlanningTaskView       `json:"tasks"`
	Budget       execution.BudgetSnapshot `json:"budget"`
	Complete     bool                     `json:"complete"`
	ObservedAt   time.Time                `json:"observed_at"`
	Capabilities PlanningCapabilities     `json:"capabilities"`
}

func planningPlanView(p planning.Plan) PlanningPlanView {
	return PlanningPlanView{p.ID, p.Revision, p.Digest, p.Objective, len(p.Tasks), p.Budget.Deadline}
}

func planningRead[T any](ctx context.Context, o *PlanningOperator, access PlanningAccess, read func(context.Context) (T, error)) (T, error) {
	var zero T
	if o == nil {
		return zero, ErrPlanningUnavailable
	}
	if cortex.ValidateScope(cortex.ScopeFromContext(ctx)) != nil {
		return zero, cortex.ErrNoScope
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := o.config.Authority.AuthorizePlanning(ctx, access); err != nil {
		return zero, errors.Join(ErrPlanningDenied, err)
	}
	return hostport.WithSynchronousFence(ctx, planning.ErrConflict, func(attempt context.Context, next func(struct{}) error) error {
		return o.config.Authority.WithPlanningReadFence(attempt, access, func() error { return next(struct{}{}) })
	}, func(attempt context.Context, _ struct{}) (T, error) {
		result, err := read(attempt)
		if err != nil {
			return zero, err
		}
		if err = o.config.Authority.AuthorizePlanning(attempt, access); err != nil {
			return zero, errors.Join(ErrPlanningDenied, err)
		}
		return result, nil
	})
}

// List returns at most 64 exact-scope graph headers after current authorization.
func (o *PlanningOperator) List(ctx context.Context, request planning.ListRequest) ([]PlanningPlanView, error) {
	if request.Limit < 1 || request.Limit > 64 || (!request.After.IsNil() && request.After.Prefix() != planning.PrefixPlan) {
		return nil, planning.ErrInvalid
	}
	return planningRead(ctx, o, PlanningAccess{Operation: "list"}, func(attempt context.Context) ([]PlanningPlanView, error) {
		plans, err := o.config.Store.ListPlans(attempt, request)
		if err != nil {
			return nil, err
		}
		if len(plans) > request.Limit {
			return nil, planning.ErrConflict
		}
		result := make([]PlanningPlanView, 0, len(plans))
		previous := request.After.String()
		for _, p := range plans {
			if planning.Validate(p, planning.Limits{}) != nil || p.Scope.Canonical() != cortex.ScopeFromContext(attempt).Canonical() || (previous != "" && p.ID.String() <= previous) {
				return nil, planning.ErrConflict
			}
			if err = o.config.Authority.AuthorizePlanning(attempt, PlanningAccess{PlanID: p.ID, Operation: "read"}); err != nil {
				return nil, errors.Join(ErrPlanningDenied, err)
			}
			result = append(result, planningPlanView(p))
			previous = p.ID.String()
		}
		return result, nil
	})
}

// Detail revalidates retained acceptance against current bindings and reports.
func (o *PlanningOperator) Detail(ctx context.Context, planID id.ID) (PlanningDetail, error) {
	if planID.IsNil() || planID.Prefix() != planning.PrefixPlan {
		return PlanningDetail{}, planning.ErrInvalid
	}
	return planningRead(ctx, o, PlanningAccess{PlanID: planID, Operation: "read"}, func(attempt context.Context) (PlanningDetail, error) { return o.detail(attempt, planID) })
}

func (o *PlanningOperator) detail(ctx context.Context, planID id.ID) (PlanningDetail, error) {
	p, states, err := o.config.Source.LoadSchedulingState(ctx, planID)
	if err != nil {
		return PlanningDetail{}, err
	}
	if p.ID != planID || planning.Validate(p, planning.Limits{}) != nil || p.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return PlanningDetail{}, planning.ErrConflict
	}
	retained, err := o.config.Store.GetPlan(ctx, planID)
	if err != nil {
		return PlanningDetail{}, err
	}
	if retained.Digest != p.Digest {
		return PlanningDetail{}, planning.ErrConflict
	}
	now := o.config.Clock.Now()
	if _, err = planning.Ready(p, states, now); err != nil {
		return PlanningDetail{}, err
	}
	byTask := make(map[id.ID]planning.TaskState, len(states))
	for _, s := range states {
		byTask[s.TaskID] = s
	}
	validUntil := make(map[id.ID]time.Time)
	detail := PlanningDetail{Plan: planningPlanView(p), ObservedAt: now, Capabilities: o.Capabilities(), Tasks: make([]PlanningTaskView, 0, len(p.Tasks))}
	for _, task := range p.Tasks {
		state, exists := byTask[task.ID]
		if !exists {
			state = planning.TaskState{TaskID: task.ID, PlanDigest: p.Digest, Status: planning.StatusPending}
		}
		row := PlanningTaskView{ID: task.ID, Owner: task.Owner, Instruction: task.Instruction, DependsOn: append([]id.ID(nil), task.DependsOn...), Status: state.Status, Budget: task.Budget, ReviewRequired: task.Review != nil, NextAction: "wait"}
		// A source's success label cannot create acceptance or reports.
		if state.Status == planning.StatusSucceeded {
			state.Status = planning.StatusBlocked
			row.Status = state.Status
		}
		claim, claimErr := o.config.Store.GetTaskClaim(ctx, planID, task.ID)
		if claimErr != nil && !errors.Is(claimErr, ErrClaimNotFound) {
			return PlanningDetail{}, claimErr
		}
		if claimErr == nil {
			if claim.Validate() != nil || claim.PlanID != p.ID || claim.TaskID != task.ID || claim.Scope.Canonical() != p.Scope.Canonical() {
				return PlanningDetail{}, planning.ErrConflict
			}
			row.ClaimID, row.ClaimRevision, row.ClaimState, row.RunID = claim.ID, claim.Revision, claim.State, claim.RunID
			if claim.PlanDigest == p.Digest {
				switch claim.State {
				case ClaimHeld:
					state.Status = planning.StatusClaimed
				case ClaimDispatching:
					state.Status = planning.StatusRunning
				case ClaimRecovering:
					state.Status = planning.StatusBlocked
				}
				row.Status = state.Status
				if claim.State == ClaimDispatching || claim.State == ClaimRecovering || claim.State == ClaimCompleted {
					dispatch, readErr := o.config.Store.GetTaskDispatch(ctx, claim.ID)
					if readErr != nil && !errors.Is(readErr, ErrClaimNotFound) {
						return PlanningDetail{}, readErr
					}
					if readErr == nil {
						assignment, readErr := o.config.Store.GetAssignment(ctx, dispatch.Assignment.Handoff.AssignmentID)
						if readErr != nil {
							return PlanningDetail{}, readErr
						}
						if assignment.Validate() != nil || assignment.Handoff.PlanID != planID || assignment.Handoff.PlanDigest != p.Digest || assignment.Handoff.TaskID != task.ID || assignment.Handoff.Fence != claim.Fence || assignment.Handoff.Scope.Canonical() != p.Scope.Canonical() {
							return PlanningDetail{}, planning.ErrConflict
						}
						row.AssignmentID, row.AssignmentRevision, row.AssignmentState = assignment.Handoff.AssignmentID, assignment.Revision, assignment.State
						switch assignment.State {
						case AssignmentCancelled:
							state.Status = planning.StatusCancelled
						case AssignmentRejected:
							state.Status = planning.StatusFailed
						case AssignmentBlocked:
							state.Status = planning.StatusBlocked
						case AssignmentSubmitted:
							state.Status = planning.StatusVerifying
						}
						row.Status = state.Status
					}
				}
				if claim.State == ClaimCompleted && claim.AcceptanceID != nil {
					accepted, readErr := o.config.Store.GetTaskAcceptance(ctx, *claim.AcceptanceID)
					if readErr != nil {
						return PlanningDetail{}, readErr
					}
					if accepted.Validate() != nil || accepted.Claim.Digest != claim.Digest {
						return PlanningDetail{}, planning.ErrConflict
					}
					row.AcceptanceID = accepted.Request.ID
					refreshed := CurrentTaskAcceptanceRequest{Accepted: AcceptedDependency{AcceptanceID: accepted.Request.ID, Digest: accepted.Digest, Current: state.Current}}
					for _, dependency := range accepted.Request.Dependencies {
						previous, dependencyErr := o.config.Store.GetTaskAcceptance(ctx, dependency.AcceptanceID)
						if dependencyErr != nil {
							return PlanningDetail{}, dependencyErr
						}
						live, found := byTask[previous.Claim.TaskID]
						if !found {
							continue
						}
						dependency.Current = live.Current
						refreshed.Dependencies = append(refreshed.Dependencies, dependency)
					}
					current, readErr := o.config.Store.ReadCurrentTaskAcceptance(ctx, refreshed)
					if readErr != nil && !errors.Is(readErr, ErrInvalidReview) && !errors.Is(readErr, ErrReviewConflict) && !errors.Is(readErr, ErrInvalidTaskAcceptance) && !errors.Is(readErr, ErrTaskAcceptanceConflict) && !errors.Is(readErr, verification.ErrConflict) && !errors.Is(readErr, verification.ErrNotFound) {
						return PlanningDetail{}, readErr
					}
					if readErr == nil && o.config.Clock.Now().Before(current.ValidUntil) {
						state = current.State
						validUntil[task.ID] = current.ValidUntil
						ref := state.Current.Candidate
						row.AcceptedArtifact = &ref
					}
				}
			}
		}
		byTask[task.ID] = state
		detail.Tasks = append(detail.Tasks, row)
	}
	verified := make([]planning.TaskState, 0, len(p.Tasks))
	for _, task := range p.Tasks {
		verified = append(verified, byTask[task.ID])
	}

	detail.Budget, err = o.config.Store.GetBudgetSnapshot(ctx, p.ID)
	if err != nil {
		return PlanningDetail{}, err
	}
	if _, _, err = detail.Budget.Policy.Freeze(); err != nil || detail.Budget.Policy.PlanID != p.ID || detail.Budget.Policy.Scope.Canonical() != p.Scope.Canonical() {
		return PlanningDetail{}, planning.ErrConflict
	}
	detail.Replans, err = o.replanViews(ctx, p)
	if err != nil {
		return PlanningDetail{}, err
	}
	detail.Artifacts, err = o.artifactViews(ctx, p)
	if err != nil {
		return PlanningDetail{}, err
	}
	latest, err := o.config.Store.GetPlan(ctx, p.ID)
	if err != nil {
		return PlanningDetail{}, err
	}
	if latest.Digest != p.Digest {
		return PlanningDetail{}, planning.ErrConflict
	}
	now = o.config.Clock.Now()
	for i := range verified {
		if verified[i].Status == planning.StatusSucceeded && !now.Before(validUntil[verified[i].TaskID]) {
			verified[i].Status = planning.StatusBlocked
		}
	}
	ready, err := planning.Ready(p, verified, now)
	if err != nil {
		return PlanningDetail{}, err
	}
	readySet := make(map[id.ID]bool, len(ready))
	for _, key := range ready {
		readySet[key] = true
	}
	detail.Complete = true
	for i := range detail.Tasks {
		row := &detail.Tasks[i]
		row.Ready = readySet[row.ID]
		row.CurrentlyAccepted = now.Before(validUntil[row.ID]) && planning.ValidateTaskAcceptance(p, row.ID, verified, now) == nil
		if row.CurrentlyAccepted {
			row.Status = planning.StatusSucceeded
			row.NextAction = "accepted"
		} else {
			detail.Complete = false
			row.AcceptedArtifact = nil
			switch {
			case row.Ready:
				row.Status = planning.StatusReady
				row.NextAction = "claim"
			case row.ClaimState == ClaimRecovering:
				row.NextAction = "reconcile"
			case row.AssignmentState == AssignmentSubmitted:
				row.NextAction = "review"
			}
		}
	}
	detail.ObservedAt = now
	return detail, nil
}
