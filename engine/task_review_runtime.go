package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/session"
	"github.com/xraph/cortex/verification"
)

// TaskReviewAction separates billable execution from retained observation.
type TaskReviewAction string

const (
	// TaskReviewExecute creates the reserved reviewer run once.
	TaskReviewExecute TaskReviewAction = "execute"
	// TaskReviewObserve reads a retained reviewer result without dispatch.
	TaskReviewObserve TaskReviewAction = "observe"
)

// TaskReviewWork resolves the exact privately retained reviewer input. Prepared
// must have no tools and a budget within the reserved handoff allowance.
type TaskReviewWork struct {
	Prepared *PreparedAgent
	Input    string
	Contract verification.Contract
}

// TaskReviewWorkResolver reads immutable prepared input without executing work.
// Honor cancellation. Resolution does not authenticate grants or reserve capacity.
type TaskReviewWorkResolver interface {
	ResolveTaskReview(context.Context, coordination.TaskCompletion) (TaskReviewWork, error)
}

// TaskReviewFence holds current plan, assignment, grants and execution authority
// through one synchronous callback. Execution must permanently exclude any other
// worker for the reserved RunID, including delayed requests and expired leases.
// Observe needs current private run/evidence read authority. Never infer authority
// from stored references or the Fresh field alone.
type TaskReviewFence interface {
	WithTaskReviewFence(context.Context, TaskReviewAction, coordination.TaskCompletion, func(coordination.TaskCompletion) error) error
}

// TaskReviewAuthorizer authenticates the current actor and independent reviewer
// under the fence. It must check current plan, producer/reviewer assignments,
// grants, budget ownership and the requested action, including historical reads.
type TaskReviewAuthorizer interface {
	AuthorizeTaskReview(context.Context, TaskReviewAction, coordination.TaskCompletion) error
}

// TaskReviewRuntimeConfig connects private resolution and explicit current authority.
// EvidenceAuthorizer separately covers reads of captured private review output.
type TaskReviewRuntimeConfig struct {
	Resolver           TaskReviewWorkResolver
	Fence              TaskReviewFence
	Authorizer         TaskReviewAuthorizer
	EvidenceAuthorizer evidence.Authorizer
}

type taskReviewRuntime struct {
	engine *Engine
	config TaskReviewRuntimeConfig
	store  taskReviewRuntimeStore
}
type taskReviewRuntimeStore interface {
	coordination.TaskCompletionStore
	GetPlan(context.Context, id.ID) (planning.Plan, error)
	GetReservation(context.Context, id.ID) (execution.Reservation, error)
}

// NewTaskReviewRuntime reuses the prepared ReAct engine for independently reserved
// reviewers. It never reserves work itself or retries an uncertain start. Reviewer
// purpose is evaluation, so ordinary learning hooks must exclude its output.
func (e *Engine) NewTaskReviewRuntime(c TaskReviewRuntimeConfig) (coordination.TaskCompletionReviewer, error) {
	if e == nil || hostport.IsNil(c.Resolver) || hostport.IsNil(c.Fence) || hostport.IsNil(c.Authorizer) || hostport.IsNil(c.EvidenceAuthorizer) {
		return nil, coordination.ErrInvalidReview
	}
	storage, ok := e.store.(taskReviewRuntimeStore)
	if !ok || e.llm == nil {
		return nil, coordination.ErrInvalidReview
	}
	return &taskReviewRuntime{engine: e, config: c, store: storage}, nil
}

func (w *taskReviewRuntime) guarded(ctx context.Context, action TaskReviewAction, c coordination.TaskCompletion, op func(context.Context, coordination.TaskCompletion) (*coordination.Review, error)) (*coordination.Review, error) {
	if c.Validate() != nil || cortex.ScopeFromContext(ctx).Canonical() != c.Request.Handoff.Scope.Canonical() {
		return nil, coordination.ErrInvalidReview
	}
	frozen, err := coordination.SealTaskCompletion(c)
	if err != nil {
		return nil, err
	}
	frozen.Fresh = c.Fresh
	deadline := time.Now().Add(30 * time.Second)
	if action == TaskReviewExecute && c.Request.Handoff.Deadline.Before(deadline) {
		deadline = c.Request.Handoff.Deadline
	}
	bounded, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	return hostport.WithSynchronousFence(bounded, coordination.ErrReviewConflict, func(attempt context.Context, next func(coordination.TaskCompletion) error) error {
		offered, sealErr := coordination.SealTaskCompletion(frozen)
		if sealErr != nil {
			return sealErr
		}
		offered.Fresh = frozen.Fresh
		return w.config.Fence.WithTaskReviewFence(attempt, action, offered, next)
	}, func(attempt context.Context, current coordination.TaskCompletion) (*coordination.Review, error) {
		if current.Validate() != nil || current.Digest != frozen.Digest || current.Fresh != frozen.Fresh {
			return nil, coordination.ErrReviewConflict
		}
		authorized, sealErr := coordination.SealTaskCompletion(frozen)
		if sealErr != nil {
			return nil, sealErr
		}
		authorized.Fresh = frozen.Fresh
		if authErr := w.config.Authorizer.AuthorizeTaskReview(attempt, action, authorized); authErr != nil {
			return nil, authErr
		}
		if contextErr := attempt.Err(); contextErr != nil {
			return nil, contextErr
		}
		retained, readErr := w.store.GetTaskCompletion(attempt, frozen.Request.AssignmentID)
		if readErr != nil {
			return nil, readErr
		}
		if retained == nil || retained.Validate() != nil || retained.Digest != frozen.Digest {
			return nil, coordination.ErrReviewConflict
		}
		return op(attempt, frozen)
	})
}

func (w *taskReviewRuntime) Execute(ctx context.Context, c coordination.TaskCompletion) error {
	if !c.Fresh {
		return cortex.ErrTaskRunReconciliationRequired
	}
	_, err := w.guarded(ctx, TaskReviewExecute, c, func(attempt context.Context, frozen coordination.TaskCompletion) (*coordination.Review, error) {
		req := frozen.Request
		reservation, readErr := w.store.GetReservation(attempt, req.Reservation.ID)
		if readErr != nil {
			return nil, readErr
		}
		if reservation.Validate() != nil || reservation.RequestDigest != frozen.Reservation.RequestDigest || reservation.Settlement != nil {
			return nil, coordination.ErrReviewConflict
		}
		offered, sealErr := coordination.SealTaskCompletion(frozen)
		if sealErr != nil {
			return nil, sealErr
		}
		work, resolveErr := w.config.Resolver.ResolveTaskReview(attempt, offered)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if work.Prepared != nil {
			work.Prepared = &PreparedAgent{payload: append([]byte(nil), work.Prepared.payload...), digest: work.Prepared.digest}
		}
		if work.Prepared == nil || work.Prepared.Digest() != req.PreparedDigest || len(work.Input) > coordination.MaxHandoffBytes || evidence.Digest([]byte(work.Input)) != req.InputDigest {
			return nil, coordination.ErrReviewConflict
		}
		snap, snapshotErr := work.Prepared.snapshot()
		if snapshotErr != nil {
			return nil, snapshotErr
		}
		budget := req.Handoff.Authority.Budget
		if snap.Scope.Canonical() != req.Handoff.Scope.Canonical() || snap.Agent.ID.String() != req.Handoff.Receiver.Agent || len(snap.Tools) != 0 || !snap.Config.ToolsRestricted || !snap.Config.AllToolsRestricted || snap.Config.Context != nil || snap.Config.Verification != nil || snap.Config.Budget.MaxSteps < 1 || snap.Config.Budget.MaxSteps > budget.MaxSteps || snap.Config.Budget.MaxTokens < 1 || snap.Config.Budget.MaxTokens > budget.MaxTokens || snap.Config.Budget.Deadline.IsZero() || snap.Config.Budget.Deadline.After(req.Handoff.Deadline) {
			return nil, coordination.ErrReviewConflict
		}
		plan, planErr := w.store.GetPlan(attempt, req.Handoff.PlanID)
		if planErr != nil {
			return nil, planErr
		}
		if coordination.ValidateHandoffPlan(req.Handoff, plan, time.Now().UTC()) != nil || planning.ValidateTaskContract(plan, req.Handoff.TaskID, work.Contract) != nil {
			return nil, coordination.ErrReviewConflict
		}
		if sessionErr := w.engine.store.CreateSession(attempt, &session.Session{ID: req.SessionID, AgentID: snap.Agent.ID, Title: "Independent task review"}); sessionErr != nil {
			return nil, sessionErr
		}
		opts := PreparedRunOptions{SessionID: req.SessionID, Purpose: run.PurposeEvaluation, AcceptanceRef: work.Contract.ID.String(), Provenance: run.ExecutionProvenance{ParentRunID: req.Handoff.ParentRunID.String(), AssignmentDigest: req.Handoff.Digest}}
		runtimeCtx, draft, prepareErr := w.engine.prepareRun(attempt, work.Prepared, work.Input, opts, req.RunID)
		if prepareErr != nil {
			return nil, prepareErr
		}
		if evidence.Digest([]byte(draft.run.Input)) != req.InputDigest {
			return nil, coordination.ErrReviewConflict
		}
		runtimeCtx, active, startErr := w.engine.activatePrepared(runtimeCtx, draft)
		if startErr != nil {
			return nil, errors.Join(cortex.ErrTaskRunReconciliationRequired, startErr)
		}
		_, runErr := w.engine.continueReAct(runtimeCtx, active.ag, active.cfg, active.run, active.state, active.startedAt)
		return nil, runErr
	})
	return err
}

func (w *taskReviewRuntime) Observe(ctx context.Context, c coordination.TaskCompletion) (*coordination.Review, error) {
	return w.guarded(ctx, TaskReviewObserve, c, func(attempt context.Context, frozen coordination.TaskCompletion) (*coordination.Review, error) {
		req := frozen.Request
		current, err := w.engine.store.GetRun(attempt, req.RunID)
		if errors.Is(err, cortex.ErrRunNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if current == nil || current.ID != req.RunID || current.SessionID != req.SessionID || current.Scope.Canonical() != req.Handoff.Scope.Canonical() || current.AgentID.String() != req.Handoff.Receiver.Agent || current.Provenance.PreparedDigest != req.PreparedDigest || current.Provenance.AssignmentDigest != req.Handoff.Digest || current.Provenance.ParentRunID != req.Handoff.ParentRunID.String() || current.Provenance.Purpose != run.PurposeEvaluation {
			return nil, coordination.ErrReviewConflict
		}
		budget := current.Budget
		ceiling := req.Handoff.Authority.Budget
		if evidence.Digest([]byte(current.Input)) != req.InputDigest || len(current.Provenance.Tools) != 0 || current.Provenance.TaskClaimID != "" || current.Usage.ToolCalls != 0 || current.Provenance.ContextSelection != nil || budget.Validate() != nil || budget.MaxSteps < 1 || budget.MaxSteps > ceiling.MaxSteps || budget.MaxTokens < 1 || budget.MaxTokens > ceiling.MaxTokens || budget.Deadline.IsZero() || budget.Deadline.After(req.Handoff.Deadline) || current.StartedAt == nil || current.StartedAt.Before(frozen.ObservedAt) || current.StartedAt.After(time.Now()) {
			return nil, coordination.ErrReviewConflict
		}
		if current.State == run.StateRunning || current.State == run.StatePaused {
			return nil, nil
		}
		if current.State != run.StateCompleted || current.StopReason != run.StopFinalAnswer || current.CompletedAt == nil || current.CompletedAt.Before(*current.StartedAt) || current.CompletedAt.After(time.Now()) {
			return nil, coordination.ErrReviewConflict
		}
		output, err := w.reviewOutput(attempt, current)
		if err != nil {
			return nil, err
		}
		var parsed coordination.ReviewOutput
		if len(output) > coordination.MaxReviewBytes || json.Unmarshal(output, &parsed) != nil {
			return nil, coordination.ErrInvalidReview
		}
		review, err := coordination.SealReview(coordination.Review{SchemaVersion: 1, ID: req.ReviewID, Handoff: req.Handoff, SubjectAssignmentID: req.AssignmentID, SubjectRevision: req.AssignmentRevision, SubjectDigest: req.AssignmentDigest, BindingDigest: req.BindingDigest, RunID: req.RunID, RunRevision: current.Revision, PreparedDigest: req.PreparedDigest, ReviewedAt: *current.CompletedAt, Findings: parsed.Findings})
		if err != nil {
			return nil, err
		}
		if outputErr := review.ValidateOutput(output); outputErr != nil {
			return nil, outputErr
		}
		latest, err := w.engine.store.GetRun(attempt, req.RunID)
		if err != nil {
			return nil, err
		}
		if latest == nil || latest.Revision != current.Revision {
			return nil, coordination.ErrReviewConflict
		}
		return &review, nil
	})
}

func (w *taskReviewRuntime) reviewOutput(ctx context.Context, r *run.Run) ([]byte, error) {
	if !r.Provenance.VerificationRequired {
		return []byte(r.Output), nil
	}
	storage, ok := w.engine.store.(interface {
		verification.CaptureIntentStore
		verification.JobStore
		evidence.Storage
	})
	if !ok {
		return nil, coordination.ErrInvalidReview
	}
	intent, err := storage.GetRunCaptureIntent(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	job, err := storage.GetVerificationJob(ctx, intent.Template.ID)
	if err != nil {
		return nil, err
	}
	if job.Request.Binding.RunID != r.ID || job.Request.Binding.Context.PreparedDigest != r.Provenance.PreparedDigest {
		return nil, coordination.ErrReviewConflict
	}
	repository, err := evidence.NewRepository(storage, w.config.EvidenceAuthorizer, coordination.MaxReviewBytes)
	if err != nil {
		return nil, err
	}
	reader, err := repository.Open(ctx, job.Request.Binding.Candidate)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, coordination.MaxReviewBytes+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if len(data) > coordination.MaxReviewBytes {
		return nil, evidence.ErrTooLarge
	}
	return data, nil
}
