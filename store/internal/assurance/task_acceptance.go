package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/verification"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
)

const (
	kindTaskAcceptance = "task_acceptance"
	kindAcceptedTask   = "accepted_task"
)

var _ coordination.TaskAcceptanceStore = (*Repository)(nil)
var _ coordination.TaskRecoveryStore = (*Repository)(nil)

// AcceptTaskResult validates retained evidence and completes ownership atomically.
func (s *Repository) AcceptTaskResult(ctx context.Context, request coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
	if request.Recovery != nil {
		return coordination.TaskAcceptance{}, coordination.ErrInvalidTaskAcceptance
	}
	return s.acceptTaskResult(ctx, request)
}

// AdoptTaskResult requires authenticated recovery proof in addition to all
// ordinary acceptance evidence. It never renews ownership or releases capacity.
func (s *Repository) AdoptTaskResult(ctx context.Context, request coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
	if request.Recovery == nil {
		return coordination.TaskAcceptance{}, coordination.ErrInvalidTaskAcceptance
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.acceptTaskResult(bounded, request)
}

func (s *Repository) acceptTaskResult(ctx context.Context, request coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
	if err := require(ctx, request.Current.Scope); err != nil {
		return coordination.TaskAcceptance{}, err
	}
	req, digest, err := request.Freeze()
	if err != nil {
		return coordination.TaskAcceptance{}, err
	}
	var result coordination.TaskAcceptance
	err = s.planTransaction(ctx, req.Claim.PlanID, func(tc context.Context, tx *Repository) error {
		result = coordination.TaskAcceptance{}
		cached, readErr := tx.GetTaskAcceptance(tc, req.ID)
		if readErr == nil {
			if cached.RequestDigest != digest {
				return coordination.ErrTaskAcceptanceConflict
			}
			result = cached
			return nil
		}
		if !errors.Is(readErr, coordination.ErrTaskAcceptanceNotFound) {
			return readErr
		}
		p, readErr := tx.GetPlan(tc, req.Claim.PlanID)
		if readErr != nil {
			return readErr
		}
		indexID := p.ID.String() + ":" + p.Digest + ":" + req.Claim.TaskID.String()
		if _, readErr = tx.get(tc, Query{Kind: kindAcceptedTask, ID: indexID}); readErr == nil {
			return coordination.ErrTaskAcceptanceConflict
		}
		if !errors.Is(readErr, ErrAbsent) {
			return readErr
		}
		claim, readErr := tx.GetTaskClaim(tc, req.Claim.PlanID, req.Claim.TaskID)
		if readErr != nil {
			return readErr
		}
		started := s.clock.Now()
		if _, claimErr := completeTaskAcceptanceClaim(claim, req, started); claimErr != nil {
			return claimErr
		}
		if claim.PlanDigest != p.Digest || claim.PlanRevision != p.Revision || claim.RunID != req.Result.RunID {
			return coordination.ErrTaskAcceptanceConflict
		}
		materials, loadErr := tx.taskAcceptanceMaterials(tc, p, req, claim)
		if loadErr != nil {
			return loadErr
		}
		observed := s.clock.Now()
		if observed.Before(started) {
			return coordination.ErrInvalidTaskAcceptance
		}
		if checkErr := validateTaskAcceptanceMaterials(p, req.Claim.TaskID, materials, observed); checkErr != nil {
			return checkErr
		}
		completed, claimErr := completeTaskAcceptanceClaim(claim, req, observed)
		if claimErr != nil {
			return claimErr
		}
		accepted, sealErr := coordination.SealTaskAcceptance(coordination.TaskAcceptance{SchemaVersion: 1, Request: req, RequestDigest: digest, Claim: completed, ObservedAt: observed, Outbox: coordination.TaskAcceptanceOutbox{EventID: id.New(coordination.PrefixTaskAcceptedEvent), Type: "task_result_accepted", AcceptanceID: req.ID, PlanID: p.ID, TaskID: claim.TaskID, RunID: claim.RunID}})
		if sealErr != nil {
			return sealErr
		}
		claimRow, rowErr := claimRecord(completed)
		if rowErr != nil {
			return rowErr
		}
		if writeErr := tx.driver.Insert(tc, claimRow); writeErr != nil {
			return writeErr
		}
		row, rowErr := record(kindTaskAcceptance, req.ID.String(), accepted.Digest, p.ID.String(), p.Scope, accepted)
		if rowErr != nil {
			return rowErr
		}
		if writeErr := tx.driver.Insert(tc, row); writeErr != nil {
			return writeErr
		}
		index, rowErr := record(kindAcceptedTask, indexID, accepted.Digest, p.ID.String(), p.Scope, req.ID)
		if rowErr != nil {
			return rowErr
		}
		if writeErr := tx.driver.Insert(tc, index); writeErr != nil {
			return writeErr
		}
		finished := s.clock.Now()
		if finished.Before(observed) {
			return coordination.ErrClaimConflict
		}
		if _, claimErr := completeTaskAcceptanceClaim(claim, req, finished); claimErr != nil {
			return claimErr
		}
		if checkErr := validateTaskAcceptanceMaterials(p, req.Claim.TaskID, materials, finished); checkErr != nil {
			return checkErr
		}
		if _, checkErr := tx.validateTaskAdjudication(tc, p, req); checkErr != nil {
			return checkErr
		}
		result = accepted
		result.Fresh = true
		return nil
	})
	if errors.Is(err, ErrExists) {
		err = coordination.ErrTaskAcceptanceConflict
	}
	if err != nil {
		return coordination.TaskAcceptance{}, err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return coordination.TaskAcceptance{}, contextErr
	}
	return result, nil
}

// GetTaskAcceptance returns a historical transition, not fresh access permission.
func (s *Repository) GetTaskAcceptance(ctx context.Context, identifier id.ID) (coordination.TaskAcceptance, error) {
	if identifier.IsNil() || identifier.Prefix() != coordination.PrefixTaskAcceptance {
		return coordination.TaskAcceptance{}, coordination.ErrInvalidTaskAcceptance
	}
	row, err := s.get(ctx, Query{Kind: kindTaskAcceptance, ID: identifier.String()})
	if errors.Is(err, ErrAbsent) {
		err = coordination.ErrTaskAcceptanceNotFound
	}
	if err != nil {
		return coordination.TaskAcceptance{}, err
	}
	return decodeTaskAcceptance(row)
}

func decodeTaskAcceptance(row Record) (coordination.TaskAcceptance, error) {
	var accepted coordination.TaskAcceptance
	if row.Deleted || len(row.Body) > coordination.MaxTaskAcceptanceBytes || json.Unmarshal([]byte(row.Body), &accepted) != nil || accepted.Validate() != nil || row.Kind != kindTaskAcceptance || row.ID != accepted.Request.ID.String() || row.Digest != accepted.Digest || row.RunID != accepted.Claim.PlanID.String() || row.ScopeCanon != accepted.Claim.Scope.Canonical() {
		return coordination.TaskAcceptance{}, coordination.ErrInvalidTaskAcceptance
	}
	return accepted, nil
}

// ListTaskAcceptances supports bounded recovery and metadata outbox discovery.
func (s *Repository) ListTaskAcceptances(ctx context.Context, req coordination.TaskAcceptanceListRequest) ([]coordination.TaskAcceptance, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if _, err := cortex.RequireScope(ctx); err != nil {
		return nil, err
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindTaskAcceptance, RunID: req.PlanID.String(), After: req.After.String(), Limit: req.Limit})
	if err != nil {
		return nil, err
	}
	result := make([]coordination.TaskAcceptance, 0, len(rows))
	for _, row := range rows {
		if checkErr := validateRecord(ctx, row); checkErr != nil {
			return nil, checkErr
		}
		accepted, decodeErr := decodeTaskAcceptance(row)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if accepted.Claim.PlanID != req.PlanID {
			return nil, coordination.ErrInvalidTaskAcceptance
		}
		result = append(result, accepted)
	}
	return result, nil
}

type taskAcceptanceMaterial struct {
	deadline time.Time
	snapshot verification.ResultSnapshot
	state    planning.TaskState
}

func validateTaskAcceptanceMaterials(p planning.Plan, taskID id.ID, materials []taskAcceptanceMaterial, at time.Time) error {
	if len(materials) == 0 || !at.Before(materials[0].deadline) {
		return coordination.ErrInvalidTaskAcceptance
	}
	states, err := validateTaskMaterials(materials, at)
	if err != nil {
		return err
	}
	return planning.ValidateTaskAcceptance(p, taskID, states, at)
}

func validateTaskMaterials(materials []taskAcceptanceMaterial, at time.Time) ([]planning.TaskState, error) {
	states := make([]planning.TaskState, 0, len(materials))
	for _, material := range materials {
		r := material.snapshot.Receipt
		read := verification.ResultReadRequest{SchemaVersion: 1, RunID: r.Request.Current.RunID, ResultID: r.Request.ResultID, ReceiptDigest: r.Digest}
		if err := material.snapshot.Validate(read, at); err != nil {
			return nil, err
		}
		states = append(states, material.state)
	}
	return states, nil
}

func (s *Repository) taskAcceptanceMaterials(ctx context.Context, p planning.Plan, req coordination.TaskAcceptanceRequest, claim coordination.Claim) ([]taskAcceptanceMaterial, error) {
	own, used, err := s.loadTaskAcceptanceMaterial(ctx, p, req, claim, 8<<20, s.clock.Now())
	if err != nil {
		return nil, err
	}
	dependencies, err := s.acceptedPrerequisiteMaterials(ctx, p, claim.TaskID, req.Dependencies, (32<<20)-used)
	if err != nil {
		return nil, err
	}
	return append([]taskAcceptanceMaterial{own}, dependencies...), nil
}

func (s *Repository) acceptedPrerequisiteMaterials(ctx context.Context, p planning.Plan, taskID id.ID, deps []coordination.AcceptedDependency, remaining int) ([]taskAcceptanceMaterial, error) {
	required := make(map[id.ID]bool)
	definitions := make(map[id.ID]planning.Task, len(p.Tasks))
	for _, task := range p.Tasks {
		definitions[task.ID] = task
	}
	var visit func(id.ID)
	visit = func(taskID id.ID) {
		for _, dependency := range definitions[taskID].DependsOn {
			if !required[dependency] {
				required[dependency] = true
				visit(dependency)
			}
		}
	}
	visit(taskID)
	if len(deps) != len(required) {
		return nil, coordination.ErrInvalidTaskAcceptance
	}
	materials := make([]taskAcceptanceMaterial, 0, len(deps))
	for _, dep := range deps {
		accepted, readErr := s.GetTaskAcceptance(ctx, dep.AcceptanceID)
		if readErr != nil {
			return nil, readErr
		}
		if accepted.Digest != dep.Digest || accepted.Claim.PlanID != p.ID || accepted.Claim.PlanDigest != p.Digest || !required[accepted.Claim.TaskID] {
			return nil, coordination.ErrInvalidTaskAcceptance
		}
		delete(required, accepted.Claim.TaskID)
		current, readErr := s.GetTaskClaim(ctx, p.ID, accepted.Claim.TaskID)
		if readErr != nil {
			return nil, readErr
		}
		if current.Digest != accepted.Claim.Digest {
			return nil, coordination.ErrTaskAcceptanceConflict
		}
		original, readErr := s.claimRevision(ctx, p.ID, current.TaskID, accepted.Request.Claim.Revision)
		if readErr != nil {
			return nil, readErr
		}
		if coordination.ValidateClaimSuccessor(original, current) != nil {
			return nil, coordination.ErrInvalidTaskAcceptance
		}
		request := accepted.Request
		request.Current = dep.Current
		material, bytes, loadErr := s.loadTaskAcceptanceMaterial(ctx, p, request, original, min(remaining, 8<<20), accepted.ObservedAt)
		if loadErr != nil {
			return nil, loadErr
		}
		remaining -= bytes
		materials = append(materials, material)
	}
	return materials, nil
}

func (s *Repository) loadTaskAcceptanceMaterial(ctx context.Context, p planning.Plan, req coordination.TaskAcceptanceRequest, claim coordination.Claim, limit int, recoveryAt time.Time) (taskAcceptanceMaterial, int, error) {
	reviewDeadline, err := s.validateTaskAdjudication(ctx, p, req)
	if err != nil {
		return taskAcceptanceMaterial{}, 0, err
	}

	assignment, err := s.GetAssignment(ctx, req.AssignmentID)
	if err != nil {
		return taskAcceptanceMaterial{}, 0, err
	}
	h, pins := assignment.Handoff, assignment.Submission
	expectedState := coordination.ClaimDispatching
	if req.Recovery != nil {
		expectedState = coordination.ClaimRecovering
	}
	if assignment.State != coordination.AssignmentSubmitted || assignment.Revision != req.AssignmentRevision || assignment.Digest != req.AssignmentDigest || pins == nil || h.PlanID != p.ID || h.PlanDigest != p.Digest || h.PlanRevision != p.Revision || h.TaskID != claim.TaskID || h.Fence != claim.Fence || h.Receiver != claim.Worker || h.GrantRef != claim.GrantRef || (req.Recovery == nil && req.Actor != h.Sender.Principal) || claim.Token() != req.Claim || claim.State != expectedState || claim.RunID != req.Result.RunID || pins.RunID != claim.RunID {
		return taskAcceptanceMaterial{}, 0, coordination.ErrInvalidTaskAcceptance
	}
	reader, ok := s.driver.(SubmissionRunReader)
	if !ok {
		return taskAcceptanceMaterial{}, 0, coordination.ErrAssignmentUnavailable
	}
	current, err := reader.LockSubmissionRun(ctx, claim.RunID)
	if err != nil {
		return taskAcceptanceMaterial{}, 0, err
	}
	closed, err := s.GetTaskUsageClosure(ctx, claim.RunID)
	if err != nil {
		return taskAcceptanceMaterial{}, 0, err
	}
	if closed != nil && closed.Request.Rejection != nil {
		return taskAcceptanceMaterial{}, 0, coordination.ErrTaskUsageClosed
	}
	job, err := s.GetVerificationJob(ctx, pins.JobID)
	if err != nil {
		return taskAcceptanceMaterial{}, 0, err
	}
	bindingDigest, err := req.Current.Digest()
	if err != nil {
		return taskAcceptanceMaterial{}, 0, err
	}
	jobDigest, err := job.Request.Binding.Digest()
	if err != nil {
		return taskAcceptanceMaterial{}, 0, err
	}
	if job.State != verification.JobCommitted || job.ResultID != req.Result.ResultID || job.RequestDigest != pins.JobRequestDigest || bindingDigest != pins.BindingDigest || bindingDigest != jobDigest || req.Current.Context.PrincipalRef != claim.Worker.Principal || current.Provenance.ParentRunID != h.ParentRunID.String() || current.Provenance.AssignmentDigest != h.Digest {
		return taskAcceptanceMaterial{}, 0, coordination.ErrInvalidTaskAcceptance
	}
	if checkErr := s.validateSubmissionRunState(ctx, current, job); checkErr != nil {
		return taskAcceptanceMaterial{}, 0, checkErr
	}
	dispatch, err := s.taskDispatchClaim(ctx, claim)
	if err != nil {
		return taskAcceptanceMaterial{}, 0, err
	}
	deadline := h.Deadline
	if !reviewDeadline.IsZero() {
		deadline = minAcceptanceDeadline(deadline, reviewDeadline)
	}
	if req.Recovery != nil {
		if current.AgentID.String() != claim.Worker.Agent || current.Provenance.TaskClaimID != claim.ID.String() || current.Provenance.PreparedDigest != req.Current.Context.PreparedDigest {
			return taskAcceptanceMaterial{}, 0, coordination.ErrInvalidTaskAcceptance
		}
		journal, lockErr := s.LockRunRecoverySnapshot(ctx, req.Recovery.Seal, req.Recovery.SnapshotDigest)
		if lockErr != nil {
			return taskAcceptanceMaterial{}, 0, lockErr
		}
		for _, entry := range journal.Calls {
			if entry.Call.StartedAt.Before(dispatch.At) {
				return taskAcceptanceMaterial{}, 0, coordination.ErrTaskAcceptanceConflict
			}
		}
		if proofErr := coordination.ValidateTaskRecoverySnapshot(*req.Recovery, journal, claim, h, req.Current, recoveryAt); proofErr != nil {
			return taskAcceptanceMaterial{}, 0, proofErr
		}
		deadline = minAcceptanceDeadline(deadline, req.Recovery.ExpiresAt)
		deadline = minAcceptanceDeadline(deadline, claim.Deadline)
	}
	snapshot, bytes, err := s.resultSnapshotForRun(ctx, req.Result, current, limit)
	if err != nil {
		return taskAcceptanceMaterial{}, 0, err
	}
	if snapshot.Receipt.ObservedAt.Before(dispatch.At) || planning.ValidateTaskBindingForContract(p, claim.TaskID, snapshot.Contract, req.Current) != nil {
		return taskAcceptanceMaterial{}, 0, coordination.ErrInvalidTaskAcceptance
	}
	state := planning.TaskState{TaskID: claim.TaskID, PlanDigest: p.Digest, Status: planning.StatusSucceeded, Current: req.Current, Reports: snapshot.Reports, Contract: &snapshot.Contract}
	return taskAcceptanceMaterial{snapshot: snapshot, state: state, deadline: deadline}, bytes, nil
}

func (s *Repository) taskDispatchClaim(ctx context.Context, claim coordination.Claim) (coordination.Claim, error) {
	row, err := s.get(ctx, Query{Kind: kindTaskDispatch, ID: claim.RunID.String()})
	if err != nil {
		return coordination.Claim{}, err
	}
	var dispatch coordination.Claim
	if row.Deleted || len(row.Body) > coordination.MaxClaimBytes || json.Unmarshal([]byte(row.Body), &dispatch) != nil || dispatch.Validate() != nil || dispatch.State != coordination.ClaimDispatching || dispatch.ID != claim.ID || dispatch.PlanID != claim.PlanID || dispatch.PlanDigest != claim.PlanDigest || dispatch.PlanRevision != claim.PlanRevision || dispatch.TaskID != claim.TaskID || dispatch.RunID != claim.RunID || dispatch.Worker != claim.Worker || dispatch.Fence != claim.Fence || dispatch.GrantRef != claim.GrantRef || dispatch.At.After(claim.At) || dispatch.Revision > claim.Revision || row.Kind != kindTaskDispatch || row.ID != claim.RunID.String() || row.Digest != dispatch.Digest || row.RunID != claimKey(claim.PlanID, claim.TaskID) || row.ScopeCanon != claim.Scope.Canonical() || dispatch.Scope.Canonical() != claim.Scope.Canonical() {
		return coordination.Claim{}, coordination.ErrInvalidClaim
	}
	return dispatch, nil
}

func completeTaskAcceptanceClaim(claim coordination.Claim, req coordination.TaskAcceptanceRequest, at time.Time) (coordination.Claim, error) {
	if req.Recovery != nil {
		return coordination.AdoptRecoveredClaim(claim, req.Claim, req.ID, *req.Recovery, at)
	}
	return coordination.CompleteClaim(claim, req.Claim, req.ID, at)
}
func minAcceptanceDeadline(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
