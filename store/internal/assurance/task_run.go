package assurance

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/run"
)

const kindTaskRunStart = "task_run_start"

// TaskRunTransaction inserts a fresh child inside the held plan transaction.
// InsertTaskRun must use insert-only semantics and roll back with all other writes.
type TaskRunTransaction interface {
	Driver
	SubmissionRunReader
	GetRun(context.Context, id.AgentRunID) (*run.Run, error)
	InsertTaskRun(context.Context, *run.Run) error
}

var _ coordination.TaskRunStore = (*Repository)(nil)

type taskRunRecord struct {
	Claim              coordination.ClaimToken   `json:"claim"`
	DispatchDigest     string                    `json:"dispatch_digest"`
	AssignmentRevision int64                     `json:"assignment_revision"`
	AssignmentDigest   string                    `json:"assignment_digest"`
	ParentRunRevision  int64                     `json:"parent_run_revision"`
	Actor              string                    `json:"actor"`
	Reason             string                    `json:"reason"`
	Result             coordination.TaskRunStart `json:"result"`
}

func (s *Repository) getTaskRunStart(ctx context.Context, claimID id.ID) (taskRunRecord, error) {
	row, err := s.get(ctx, Query{Kind: kindTaskRunStart, ID: claimID.String()})
	if err != nil {
		return taskRunRecord{}, err
	}
	var retained taskRunRecord
	if row.Deleted || len(row.Body) > 64<<10 || json.Unmarshal([]byte(row.Body), &retained) != nil || retained.Claim.Validate() != nil || retained.ParentRunRevision < 1 || retained.Actor != retained.Claim.Worker.Principal || retained.Reason == "" || len(retained.Reason) > 4096 || row.Kind != kindTaskRunStart || row.ID != retained.Claim.ClaimID.String() || row.ID != claimID.String() || row.RunID != retained.Claim.PlanID.String() || row.Digest != evidence.Digest([]byte(row.Body)) {
		return taskRunRecord{}, coordination.ErrInvalidClaim
	}
	result, out := retained.Result, retained.Result.Outbox
	if !evidence.ValidDigest(result.RequestDigest) || !evidence.ValidDigest(result.RunDigest) || !evidence.ValidDigest(result.IntentDigest) || result.CreatedAt.IsZero() || out.EventID.IsNil() || out.EventID.Prefix() != coordination.PrefixTaskRunEvent || out.Type != "task_run_created" || out.ClaimID != claimID || out.PlanID != retained.Claim.PlanID || out.TaskID != retained.Claim.TaskID || out.RunID != result.RunID || out.IntentID != result.IntentID {
		return taskRunRecord{}, coordination.ErrInvalidClaim
	}
	req := retained
	dispatched, err := s.getTaskDispatch(ctx, claimID)
	if err != nil {
		return taskRunRecord{}, err
	}
	claim, err := s.claimRevision(ctx, req.Claim.PlanID, req.Claim.TaskID, req.Claim.Revision)
	if err != nil {
		return taskRunRecord{}, err
	}
	if claim.Scope.Canonical() != row.ScopeCanon || claim.Token() != req.Claim || claim.State != coordination.ClaimDispatching || claim.RunID != result.RunID || dispatched.Result.RequestDigest != req.DispatchDigest || dispatched.Result.Claim.RunID != result.RunID || dispatched.Result.Assignment.Digest != req.AssignmentDigest || dispatched.Result.Assignment.Revision != req.AssignmentRevision {
		return taskRunRecord{}, coordination.ErrInvalidClaim
	}
	intent, err := s.GetRunCaptureIntent(ctx, result.RunID)
	if err != nil {
		return taskRunRecord{}, err
	}
	if intent.ID != result.IntentID || intent.Digest != retained.Result.IntentDigest {
		return taskRunRecord{}, coordination.ErrInvalidClaim
	}
	return retained, nil
}

// CreateTaskRun creates the reserved child and capture policy under live claim,
// parent, assignment and prerequisite fences. Hosts supply current authorization
// and external revision guards. This operation performs no external execution.
func (s *Repository) CreateTaskRun(ctx context.Context, request coordination.TaskRunRequest) (coordination.TaskRunStart, error) {
	if err := require(ctx, request.Run.Scope); err != nil {
		return coordination.TaskRunStart{}, err
	}
	req, digest, err := request.Freeze()
	if err != nil {
		return coordination.TaskRunStart{}, err
	}
	if req.Intent.MaxCandidateBytes > MaxEvidenceBytes || req.Intent.MaxTranscriptBytes > MaxEvidenceBytes {
		return coordination.TaskRunStart{}, evidence.ErrTooLarge
	}
	var result coordination.TaskRunStart
	err = s.planTransaction(ctx, req.Claim.PlanID, func(tc context.Context, tx *Repository) error {
		result = coordination.TaskRunStart{}
		cached, readErr := tx.getTaskRunStart(tc, req.Claim.ClaimID)
		if readErr == nil {
			if cached.Result.RequestDigest != digest {
				return coordination.ErrClaimConflict
			}
			result = cached.Result
			return nil
		}
		if !errors.Is(readErr, ErrAbsent) {
			return readErr
		}
		driver, ok := tx.driver.(TaskRunTransaction)
		if !ok {
			return coordination.ErrAssignmentUnavailable
		}
		dispatch, readErr := tx.getTaskDispatch(tc, req.Claim.ClaimID)
		if readErr != nil {
			return readErr
		}
		original := dispatch.Result
		if original.RequestDigest != req.DispatchDigest || original.Claim.RunID != req.Run.ID || original.Assignment.Digest != req.AssignmentDigest || original.Assignment.Revision != req.AssignmentRevision {
			return coordination.ErrClaimConflict
		}
		claim, readErr := tx.GetTaskClaim(tc, req.Claim.PlanID, req.Claim.TaskID)
		if readErr != nil {
			return readErr
		}
		if claim.Token() != req.Claim || claim.State != coordination.ClaimDispatching || claim.RunID != req.Run.ID || claim.GrantRef != original.Claim.GrantRef || claim.PlanDigest != original.Claim.PlanDigest || claim.PlanRevision != original.Claim.PlanRevision {
			return coordination.ErrClaimConflict
		}
		p, readErr := tx.GetPlan(tc, claim.PlanID)
		if readErr != nil {
			return readErr
		}
		if p.Digest != claim.PlanDigest || p.Revision != claim.PlanRevision {
			return coordination.ErrClaimConflict
		}
		assignment, readErr := tx.GetAssignment(tc, original.Assignment.Handoff.AssignmentID)
		if readErr != nil {
			return readErr
		}
		h := assignment.Handoff
		if assignment.State != coordination.AssignmentWorking || assignment.Revision != req.AssignmentRevision || assignment.Digest != req.AssignmentDigest || req.Run.Provenance.AssignmentDigest != h.Digest || req.Run.Provenance.ParentRunID != h.ParentRunID.String() || req.Run.AgentID.String() != claim.Worker.Agent {
			return coordination.ErrClaimConflict
		}
		parent, readErr := tx.lockTaskParent(tc, h.ParentRunID, req.ParentRunRevision, req.Run.Scope)
		if readErr != nil {
			return readErr
		}
		started := s.clock.Now()
		if started.IsZero() || started.Before(claim.At) || !started.Before(claim.ExpiresAt) || !beforeOptionalDeadline(started, parent.Budget.Deadline) {
			return coordination.ErrClaimConflict
		}
		if checkErr := coordination.ValidateHandoffPlan(h, p, started); checkErr != nil {
			return checkErr
		}
		if checkErr := tx.checkClaimBudget(tc, claim, started); checkErr != nil {
			return checkErr
		}
		budget, readErr := tx.getClaimBudget(tc, claim.ID)
		if readErr != nil {
			return readErr
		}
		b, allocation := req.Run.Budget, budget.Result.Reservation.Request.Allocation
		ceiling := h.Authority.Budget
		if b.MaxSteps > ceiling.MaxSteps || b.MaxTokens > ceiling.MaxTokens || b.MaxToolCalls > ceiling.MaxToolCalls || b.Deadline.After(ceiling.Deadline) || b.Deadline.After(budget.Result.Reservation.Request.Deadline) || (!parent.Budget.Deadline.IsZero() && b.Deadline.After(parent.Budget.Deadline)) || !allocation.Covers(execution.Resources{Steps: int64(b.MaxSteps), Tokens: int64(b.MaxTokens), ToolCalls: int64(b.MaxToolCalls), FanOut: 1}) {
			return execution.ErrBudgetExceeded
		}
		contract, readErr := tx.GetContract(tc, req.Intent.Template.ContractID)
		if readErr != nil {
			return readErr
		}
		if checkErr := planning.ValidateTaskBindingForContract(p, claim.TaskID, contract, req.Intent.Template.Binding); checkErr != nil {
			return checkErr
		}
		for _, task := range p.Tasks {
			if task.ID == claim.TaskID && req.Intent.MaxCandidateBytes > task.Output.MaxBytes {
				return coordination.ErrInvalidClaim
			}
		}
		materials, loadErr := tx.acceptedPrerequisiteMaterials(tc, p, claim.TaskID, req.Dependencies, 32<<20)
		if loadErr != nil {
			return loadErr
		}
		observed := s.clock.Now()
		if observed.Before(started) {
			return coordination.ErrClaimConflict
		}
		if checkErr := validateReadyClaimMaterials(p, claim.TaskID, materials, observed); checkErr != nil {
			return checkErr
		}
		child, buildErr := req.InitialRun(observed)
		if buildErr != nil {
			return buildErr
		}
		if insertErr := driver.InsertTaskRun(tc, &child); insertErr != nil {
			return insertErr
		}
		if registerErr := tx.createCaptureIntentInTransaction(tc, driver, req.Intent); registerErr != nil {
			return registerErr
		}
		runData, encodeErr := json.Marshal(child)
		if encodeErr != nil {
			return encodeErr
		}
		admitted := coordination.TaskRunStart{RequestDigest: digest, RunID: child.ID, RunDigest: evidence.Digest(runData), CreatedAt: child.CreatedAt, IntentID: req.Intent.ID, IntentDigest: req.Intent.Digest, Outbox: coordination.TaskRunOutbox{EventID: id.New(coordination.PrefixTaskRunEvent), Type: "task_run_created", ClaimID: claim.ID, PlanID: claim.PlanID, TaskID: claim.TaskID, RunID: child.ID, IntentID: req.Intent.ID}}
		if checkErr := admitted.Validate(req); checkErr != nil {
			return checkErr
		}
		retained := taskRunRecord{Claim: req.Claim, DispatchDigest: req.DispatchDigest, AssignmentRevision: req.AssignmentRevision, AssignmentDigest: req.AssignmentDigest, ParentRunRevision: req.ParentRunRevision, Actor: req.Actor, Reason: req.Reason, Result: admitted}
		data, encodeErr := json.Marshal(retained)
		if encodeErr != nil {
			return encodeErr
		}
		if len(data) > 64<<10 {
			return evidence.ErrTooLarge
		}
		row, buildErr := record(kindTaskRunStart, claim.ID.String(), evidence.Digest(data), claim.PlanID.String(), child.Scope, retained)
		if buildErr != nil {
			return buildErr
		}
		if insertErr := tx.driver.Insert(tc, row); insertErr != nil {
			return insertErr
		}
		finished := s.clock.Now()
		if finished.Before(observed) || !finished.Before(claim.ExpiresAt) || !finished.Before(child.Budget.Deadline) || !finished.Before(req.Intent.Template.Deadline) || !beforeOptionalDeadline(finished, parent.Budget.Deadline) {
			return coordination.ErrClaimConflict
		}
		if checkErr := validateReadyClaimMaterials(p, claim.TaskID, materials, finished); checkErr != nil {
			return checkErr
		}
		result = admitted
		result.Fresh = true
		return nil
	})
	if errors.Is(err, ErrExists) {
		err = coordination.ErrClaimConflict
	}
	if err != nil {
		return coordination.TaskRunStart{}, err
	}
	return result, nil
}

// GetTaskRunStart reads the original creation metadata, never execution
// permission or the child's latest state. Read the current run for recovery.
func (s *Repository) GetTaskRunStart(ctx context.Context, claimID id.ID) (coordination.TaskRunStart, error) {
	if claimID.IsNil() || claimID.Prefix() != coordination.PrefixClaim {
		return coordination.TaskRunStart{}, coordination.ErrInvalidClaim
	}
	retained, err := s.getTaskRunStart(ctx, claimID)
	if errors.Is(err, ErrAbsent) {
		err = coordination.ErrClaimNotFound
	}
	if err != nil {
		return coordination.TaskRunStart{}, err
	}
	return retained.Result, nil
}
