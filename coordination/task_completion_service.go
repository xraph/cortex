package coordination

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/planning"
)

// TaskCompletionServiceStore supplies durable producer and reviewer records.
type TaskCompletionServiceStore interface {
	AssignmentStore
	ReviewStore
	TaskAdjudicationStore
	TaskCompletionStore
	TaskCompletionSettlementStore
	GetPlan(context.Context, id.ID) (planning.Plan, error)
	GetTaskAcceptance(context.Context, id.ID) (TaskAcceptance, error)
}

// TaskCompletionResolver resolves already committed producer evidence, independent
// prepared review work, and current acceptance bindings. Nil means pending. Never
// execute agents or mutate storage here. Requests must retain stable identities
// across process restarts. Acceptance may carry authenticated recovery proof.
type TaskCompletionResolver interface {
	ResolveTaskSubmission(context.Context, Assignment) (*Submission, error)
	ResolveTaskCompletion(context.Context, Assignment) (*TaskCompletionRequest, error)
	ResolveTaskAcceptance(context.Context, TaskCompletion, TaskAdjudication) (*TaskAcceptanceRequest, error)
}

// TaskCompletionAuthority authenticates the coordinator and guards producer and
// required reviewer policy, grants and external revisions for one bounded pass.
// Authorize covers submission, reservation, advisory review recording and derived
// adjudication. The fence must invoke its callback once synchronously and keep
// these guards through return. Reviewer execution and task acceptance also enforce
// their own current authority. Store metadata alone is never sufficient.
type TaskCompletionAuthority interface {
	WithTaskCompletionFence(context.Context, Assignment, func(Assignment) error) error
	AuthorizeTaskCompletion(context.Context, Assignment) error
}

// TaskCompletionAcceptance routes ordinary completion to a guarded TaskAcceptor
// and expired ownership to a guarded TaskAdopter with authenticated recovery proof.
// It must not synthesize proof or renew an expired producer merely to accept it.
type TaskCompletionAcceptance interface {
	Accept(context.Context, TaskAcceptanceRequest) (TaskAcceptance, error)
}

// ScheduledTaskCompletion identifies a producer assignment without granting access.
type ScheduledTaskCompletion struct {
	Scope        cortex.Scope
	AssignmentID id.ID
}

// TaskCompletionListRequest bounds host discovery using an opaque cursor.
type TaskCompletionListRequest struct {
	After string
	Limit int
}

// TaskCompletionPage ends a discovery pass when Next is empty.
type TaskCompletionPage struct {
	Assignments []ScheduledTaskCompletion
	Next        string
}

// TaskCompletionSource authorizes discovery of producer assignments, including
// working assignments whose committed result has not been submitted yet.
// Honor cancellation, finish each pass and never invoke providers here.
type TaskCompletionSource interface {
	ListTaskCompletionAssignments(context.Context, TaskCompletionListRequest) (TaskCompletionPage, error)
}

// TaskCompletionServiceConfig bounds an independently scheduled completion loop.
// Discovery authorizes assignment metadata reads. Pending review returns immediately, leaving
// worker execution slots free. An optional Abandoner requires a resolver implementing
// TaskCompletionAbandonmentResolver and an explicit proof-authorized closer.
// Callbacks must honor cancellation.
type TaskCompletionServiceConfig struct {
	Store                                      TaskCompletionServiceStore
	Discovery                                  TaskCompletionSource
	Resolver                                   TaskCompletionResolver
	Authority                                  TaskCompletionAuthority
	Reviewer                                   TaskCompletionReviewer
	Acceptor                                   TaskCompletionAcceptance
	Abandoner                                  TaskCompletionAbandoner
	Interval, DiscoveryTimeout, AttemptTimeout time.Duration
	MaxInFlight, MaxPerScope                   int
	ReportError                                func(error)
}

// TaskCompletionService owns bounded completion attempts and joined shutdown.
// A retained intent is reconciliation-only after any uncertain dispatch.
type TaskCompletionService struct{ service *TaskService }

// NewTaskCompletionService validates required host ports without starting work.
// Attempts default to one minute and cannot exceed thirty minutes. Discovery and
// fairness use the shared lifecycle with four concurrent attempts, one per scope.
func NewTaskCompletionService(config TaskCompletionServiceConfig) (*TaskCompletionService, error) {
	if hostport.IsNil(config.Discovery) || hostport.IsNil(config.Store) || hostport.IsNil(config.Resolver) || hostport.IsNil(config.Authority) || hostport.IsNil(config.Reviewer) || hostport.IsNil(config.Acceptor) {
		return nil, ErrInvalidScheduler
	}
	if !hostport.IsNil(config.Abandoner) {
		if _, ok := config.Resolver.(TaskCompletionAbandonmentResolver); !ok {
			return nil, ErrInvalidScheduler
		}
	}
	if config.AttemptTimeout == 0 {
		config.AttemptTimeout = time.Minute
	}
	if config.AttemptTimeout < time.Millisecond || config.AttemptTimeout > 30*time.Minute {
		return nil, ErrInvalidScheduler
	}
	service, err := newIdentityOperation(PrefixAssignment, TaskServiceConfig{Discovery: completionPlanSource{config.Discovery}, Interval: config.Interval, DiscoveryTimeout: config.DiscoveryTimeout, MaxInFlight: config.MaxInFlight, Scheduler: SchedulerConfig{MaxPerScope: config.MaxPerScope}, ReportError: config.ReportError}, config.AttemptTimeout, func(ctx context.Context, key id.ID) error { return completeAssignment(ctx, config, key) })
	if err != nil {
		return nil, err
	}
	return &TaskCompletionService{service: service}, nil
}

// Start owns discovery and preserves the host's service identity.
func (s *TaskCompletionService) Start(ctx context.Context) error { return s.service.Start(ctx) }

// RequestStop cancels discovery and every outstanding completion attempt.
func (s *TaskCompletionService) RequestStop() { s.service.RequestStop() }

// Stop waits for every owned callback before storage can be disposed.
func (s *TaskCompletionService) Stop(ctx context.Context) error { return s.service.Stop(ctx) }

// Status reports process counters without assignment content.
func (s *TaskCompletionService) Status() TaskServiceStatus { return s.service.Status() }

func completeAssignment(ctx context.Context, config TaskCompletionServiceConfig, key id.ID) error {
	subject, err := config.Store.GetAssignment(ctx, key)
	if err != nil {
		return err
	}
	scope, scopeErr := cortex.RequireScope(ctx)
	if scopeErr != nil {
		return scopeErr
	}
	if subject.Validate() != nil || subject.Handoff.AssignmentID != key || subject.Handoff.Scope.Canonical() != scope.Canonical() {
		return ErrInvalidAssignment
	}
	if subject.Handoff.Purpose != HandoffWork || (subject.State != AssignmentWorking && subject.State != AssignmentSubmitted) {
		return nil
	}
	frozen, err := sealAssignment(subject)
	if err != nil {
		return err
	}
	_, err = hostport.WithSynchronousFence(ctx, ErrInvalidReview, func(attempt context.Context, next func(Assignment) error) error {
		return config.Authority.WithTaskCompletionFence(attempt, frozen, next)
	}, func(attempt context.Context, current Assignment) (struct{}, error) {
		if current.Validate() != nil || current.Digest != subject.Digest {
			return struct{}{}, ErrAssignmentConflict
		}
		authorized, freezeErr := sealAssignment(current)
		if freezeErr != nil {
			return struct{}{}, freezeErr
		}
		if authErr := config.Authority.AuthorizeTaskCompletion(attempt, authorized); authErr != nil {
			return struct{}{}, authErr
		}
		if contextErr := attempt.Err(); contextErr != nil {
			return struct{}{}, contextErr
		}
		return struct{}{}, finishAssignment(attempt, config, subject)
	})
	return err
}

func finishAssignment(ctx context.Context, config TaskCompletionServiceConfig, subject Assignment) error {
	if subject.State == AssignmentWorking {
		offered, err := sealAssignment(subject)
		if err != nil {
			return err
		}
		submission, err := config.Resolver.ResolveTaskSubmission(ctx, offered)
		if err != nil {
			return err
		}
		if submission == nil {
			return ctx.Err()
		}
		subject, err = config.Store.AdvanceAssignment(ctx, AssignmentChange{AssignmentID: subject.Handoff.AssignmentID, ExpectedRevision: subject.Revision, HandoffDigest: subject.Handoff.Digest, Fence: subject.Handoff.Fence, State: AssignmentSubmitted, Actor: subject.Handoff.Receiver.Principal, Reason: "committed producer evidence", Submission: submission})
		if err != nil {
			return err
		}
	}
	completion, err := config.Store.GetTaskCompletion(ctx, subject.Handoff.AssignmentID)
	if err != nil {
		return err
	}
	if completion != nil {
		completion.Fresh = false
	}
	if completion == nil {
		offered, freezeErr := sealAssignment(subject)
		if freezeErr != nil {
			return freezeErr
		}
		request, resolveErr := config.Resolver.ResolveTaskCompletion(ctx, offered)
		if resolveErr != nil {
			return resolveErr
		}
		if request == nil {
			return ctx.Err()
		}
		if request.AssignmentID != subject.Handoff.AssignmentID || request.AssignmentDigest != subject.Digest || request.AssignmentRevision != subject.Revision {
			return ErrInvalidReview
		}
		frozen, digest, freezeErr := request.Freeze()
		if freezeErr != nil {
			return freezeErr
		}
		admitted, admitErr := config.Store.ReserveTaskCompletion(ctx, frozen)
		if admitErr != nil {
			return admitErr
		}
		if admitted.Validate() != nil || admitted.RequestDigest != digest {
			return ErrInvalidReview
		}
		completion = &admitted
	}
	if completion.Validate() != nil || completion.Request.AssignmentID != subject.Handoff.AssignmentID || completion.Request.AssignmentDigest != subject.Digest || completion.Request.AssignmentRevision != subject.Revision {
		return ErrInvalidReview
	}
	retained, err := config.Store.GetTaskAcceptance(ctx, completion.Request.AcceptanceID)
	if err == nil {
		if retained.Validate() != nil || retained.Request.AssignmentDigest != subject.Digest {
			return ErrInvalidTaskAcceptance
		}
		return nil
	}
	if !errors.Is(err, ErrTaskAcceptanceNotFound) {
		return err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if completion.Fresh {
		offered, freezeErr := SealTaskCompletion(*completion)
		if freezeErr != nil {
			return freezeErr
		}
		offered.Fresh = true
		if executeErr := config.Reviewer.Execute(ctx, offered); executeErr != nil {
			return executeErr
		}
	}
	settled, settleErr := config.Store.SettleTaskCompletion(ctx, completion.Request.AssignmentID, completion.Digest)
	if settleErr != nil {
		return settleErr
	}
	if settled == nil && !hostport.IsNil(config.Abandoner) {
		resolver, ok := config.Resolver.(TaskCompletionAbandonmentResolver)
		if !ok {
			return ErrInvalidScheduler
		}
		request, resolveErr := resolver.ResolveTaskCompletionAbandonment(ctx, *completion)
		if resolveErr != nil {
			return resolveErr
		}
		if request != nil {
			if request.Completion.Digest != completion.Digest {
				return ErrReviewConflict
			}
			closed, closeErr := config.Abandoner.Abandon(ctx, *request)
			if closeErr != nil {
				return closeErr
			}
			settled = &closed
		}
	}
	if settled != nil && (settled.Validate() != nil || settled.CompletionDigest != completion.Digest || settled.RunID != completion.Request.RunID) {
		return ErrInvalidReview
	}
	if settled != nil && settled.Abandonment != nil {
		return nil
	}
	offered, err := SealTaskCompletion(*completion)
	if err != nil {
		return err
	}
	review, err := config.Reviewer.Observe(ctx, offered)
	if err != nil {
		return err
	}
	if review == nil {
		return ctx.Err()
	}
	req := completion.Request
	if review.Validate() != nil || review.ID != req.ReviewID || review.RunID != req.RunID || review.Handoff.Digest != req.Handoff.Digest || review.PreparedDigest != req.PreparedDigest || review.SubjectAssignmentID != req.AssignmentID || review.SubjectRevision != req.AssignmentRevision || review.SubjectDigest != req.AssignmentDigest || review.BindingDigest != req.BindingDigest {
		return ErrInvalidReview
	}
	saved, err := config.Store.RecordAssignmentReview(ctx, *review)
	if err != nil {
		return err
	}
	if saved.Validate() != nil || saved.Digest != review.Digest {
		return ErrInvalidReview
	}
	p, err := config.Store.GetPlan(ctx, req.Handoff.PlanID)
	if err != nil {
		return err
	}
	var policy *planning.ReviewRequirement
	for _, task := range p.Tasks {
		if task.ID == req.Handoff.TaskID {
			policy = task.Review
			break
		}
	}
	if policy == nil {
		return ErrInvalidReview
	}
	adjudicationRequest := TaskAdjudicationRequest{SchemaVersion: 1, ID: req.AdjudicationID, PlanID: req.Handoff.PlanID, PlanDigest: req.Handoff.PlanDigest, TaskID: req.Handoff.TaskID, AssignmentID: req.AssignmentID, AssignmentRevision: req.AssignmentRevision, AssignmentDigest: req.AssignmentDigest, BindingDigest: req.BindingDigest, ReviewID: saved.ID, ReviewDigest: saved.Digest, PolicyRevision: policy.PolicyRevision, Actor: req.Actor, Reason: "required independent review"}
	_, adjudicationDigest, err := adjudicationRequest.Freeze()
	if err != nil {
		return err
	}
	adjudication, err := config.Store.RecordTaskAdjudication(ctx, adjudicationRequest)
	if err != nil {
		return err
	}
	if adjudication.Validate() != nil || adjudication.RequestDigest != adjudicationDigest {
		return ErrInvalidReview
	}
	if adjudication.Decision != AdjudicationSupported {
		return nil
	}
	offered, err = SealTaskCompletion(*completion)
	if err != nil {
		return err
	}
	accept, err := config.Resolver.ResolveTaskAcceptance(ctx, offered, adjudication)
	if err != nil {
		return err
	}
	if accept == nil {
		return ctx.Err()
	}
	if accept.SchemaVersion != 2 || accept.ID != req.AcceptanceID || accept.AssignmentID != req.AssignmentID || accept.AssignmentRevision != req.AssignmentRevision || accept.AssignmentDigest != req.AssignmentDigest || accept.Adjudication == nil || accept.Adjudication.ID != adjudication.Request.ID || accept.Adjudication.Digest != adjudication.Digest {
		return ErrInvalidTaskAcceptance
	}
	frozen, digest, err := accept.Freeze()
	if err != nil {
		return err
	}
	accepted, err := config.Acceptor.Accept(ctx, frozen)
	if err != nil {
		return err
	}
	_, err = checkedTaskAcceptance(accepted, digest)
	return err
}

type completionPlanSource struct{ source TaskCompletionSource }

func (s completionPlanSource) ListScheduledPlans(ctx context.Context, req TaskPlanListRequest) (TaskPlanPage, error) {
	if hostport.IsNil(s.source) {
		return TaskPlanPage{}, ErrInvalidScheduler
	}
	page, err := s.source.ListTaskCompletionAssignments(ctx, TaskCompletionListRequest(req))
	if err != nil {
		return TaskPlanPage{}, err
	}
	if len(page.Assignments) > req.Limit {
		return TaskPlanPage{}, ErrInvalidScheduler
	}
	result := TaskPlanPage{Next: page.Next, Plans: make([]ScheduledPlan, 0, len(page.Assignments))}
	for _, a := range page.Assignments {
		result.Plans = append(result.Plans, ScheduledPlan{Scope: a.Scope, PlanID: a.AssignmentID})
	}
	return result, nil
}
