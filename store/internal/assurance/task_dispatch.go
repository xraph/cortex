package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

const kindReadyTaskDispatch = "task_ready_dispatch"

var _ coordination.TaskDispatchStore = (*Repository)(nil)

type taskDispatchRecord struct {
	Request coordination.TaskDispatchRequest `json:"request"`
	Result  coordination.TaskDispatch        `json:"result"`
}

func (s *Repository) getTaskDispatch(ctx context.Context, claimID id.ID) (taskDispatchRecord, error) {
	row, err := s.get(ctx, Query{Kind: kindReadyTaskDispatch, ID: claimID.String()})
	if err != nil {
		return taskDispatchRecord{}, err
	}
	var retained taskDispatchRecord
	if row.Deleted || len(row.Body) > 2<<20 || json.Unmarshal([]byte(row.Body), &retained) != nil || retained.Result.Validate(retained.Request) != nil || retained.Request.Claim.ClaimID != claimID || row.Kind != kindReadyTaskDispatch || row.ID != claimID.String() || row.RunID != retained.Request.Claim.PlanID.String() || row.ScopeCanon != retained.Request.Scope.Canonical() || row.Digest != evidence.Digest([]byte(row.Body)) {
		return taskDispatchRecord{}, coordination.ErrInvalidClaim
	}
	req := retained.Request
	previous, err := s.claimRevision(ctx, req.Claim.PlanID, req.Claim.TaskID, req.Claim.Revision)
	if err != nil {
		return taskDispatchRecord{}, err
	}
	if previous.Digest != req.ClaimDigest || coordination.ValidateClaimSuccessor(previous, retained.Result.Claim) != nil {
		return taskDispatchRecord{}, coordination.ErrInvalidClaim
	}
	dispatch, err := s.taskDispatchClaim(ctx, retained.Result.Claim)
	if err != nil {
		return taskDispatchRecord{}, err
	}
	storedClaim, err := s.claimRevision(ctx, req.Claim.PlanID, req.Claim.TaskID, req.Claim.Revision+1)
	if err != nil {
		return taskDispatchRecord{}, err
	}
	if storedClaim.Digest != retained.Result.Claim.Digest {
		return taskDispatchRecord{}, coordination.ErrInvalidClaim
	}
	priorAssignment, err := s.assignmentRevision(ctx, req.AssignmentID, req.AssignmentRevision)
	if err != nil {
		return taskDispatchRecord{}, err
	}
	if priorAssignment.State != coordination.AssignmentAccepted || priorAssignment.Digest != req.AssignmentDigest {
		return taskDispatchRecord{}, coordination.ErrInvalidClaim
	}
	rebuilt, err := coordination.Advance(priorAssignment, coordination.AssignmentChange{AssignmentID: req.AssignmentID, ExpectedRevision: req.AssignmentRevision, HandoffDigest: priorAssignment.Handoff.Digest, Fence: req.Claim.Fence, State: coordination.AssignmentWorking, Actor: req.Actor, Reason: req.Reason}, retained.Result.Claim.At)
	if err != nil || rebuilt.Digest != retained.Result.Assignment.Digest {
		return taskDispatchRecord{}, coordination.ErrInvalidClaim
	}
	assignment, err := s.assignmentRevision(ctx, req.AssignmentID, req.AssignmentRevision+1)
	if err != nil {
		return taskDispatchRecord{}, err
	}
	admission, err := s.getReadyClaim(ctx, claimID)
	if err != nil {
		return taskDispatchRecord{}, err
	}
	if dispatch.Digest != retained.Result.Claim.Digest || assignment.Digest != retained.Result.Assignment.Digest || admission.Result.RequestDigest != req.AdmissionDigest {
		return taskDispatchRecord{}, coordination.ErrInvalidClaim
	}
	return retained, nil
}

// DispatchReadyTask checks current retained readiness, ownership, assignment and
// parent state before committing intent. The caller must hold current host grant
// and external revision guards. No transaction invokes the agent runtime.
func (s *Repository) DispatchReadyTask(ctx context.Context, request coordination.TaskDispatchRequest) (coordination.TaskDispatch, error) {
	if err := require(ctx, request.Scope); err != nil {
		return coordination.TaskDispatch{}, err
	}
	req, digest, err := request.Freeze()
	if err != nil {
		return coordination.TaskDispatch{}, err
	}
	var result coordination.TaskDispatch
	err = s.planTransaction(ctx, req.Claim.PlanID, func(tc context.Context, tx *Repository) error {
		result = coordination.TaskDispatch{}
		cached, readErr := tx.getTaskDispatch(tc, req.Claim.ClaimID)
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
		admission, readErr := tx.getReadyClaim(tc, req.Claim.ClaimID)
		if readErr != nil {
			return readErr
		}
		if admission.Result.RequestDigest != req.AdmissionDigest {
			return coordination.ErrClaimConflict
		}
		current, readErr := tx.GetTaskClaim(tc, req.Claim.PlanID, req.Claim.TaskID)
		if readErr != nil {
			return readErr
		}
		original := admission.Result.Acquisition.Claim
		if current.State != coordination.ClaimHeld || current.Token() != req.Claim || current.Digest != req.ClaimDigest || current.ID != original.ID || current.PlanDigest != original.PlanDigest || current.GrantRef != original.GrantRef {
			return coordination.ErrClaimConflict
		}
		p, readErr := tx.GetPlan(tc, req.Claim.PlanID)
		if readErr != nil {
			return readErr
		}
		if p.Digest != current.PlanDigest || p.Revision != current.PlanRevision {
			return coordination.ErrClaimConflict
		}
		previous, readErr := tx.GetAssignment(tc, req.AssignmentID)
		if readErr != nil {
			return readErr
		}
		h := previous.Handoff
		if previous.State != coordination.AssignmentAccepted || previous.Revision != req.AssignmentRevision || previous.Digest != req.AssignmentDigest || h.Purpose != coordination.HandoffWork || h.PlanID != current.PlanID || h.PlanRevision != current.PlanRevision || h.PlanDigest != current.PlanDigest || h.TaskID != current.TaskID || h.Scope.Canonical() != current.Scope.Canonical() || h.Receiver != current.Worker || h.GrantRef != current.GrantRef || h.Fence != current.Fence || h.ParentRunID == req.RunID || current.ExpiresAt.After(h.Deadline) {
			return coordination.ErrClaimConflict
		}
		reader, ok := tx.driver.(SubmissionRunReader)
		if !ok {
			return coordination.ErrAssignmentUnavailable
		}
		parent, readErr := reader.LockSubmissionRun(tc, h.ParentRunID)
		if readErr != nil {
			return readErr
		}
		if parent == nil || parent.ID != h.ParentRunID || parent.Revision != req.ParentRunRevision || parent.Scope.Canonical() != req.Scope.Canonical() || (parent.State != run.StateRunning && parent.State != run.StatePaused) || (!parent.Budget.Deadline.IsZero() && current.ExpiresAt.After(parent.Budget.Deadline)) {
			return coordination.ErrClaimConflict
		}
		started := s.clock.Now()
		if checkErr := coordination.ValidateHandoffPlan(h, p, started); checkErr != nil {
			return checkErr
		}
		materials, loadErr := tx.acceptedPrerequisiteMaterials(tc, p, current.TaskID, req.Dependencies, 32<<20)
		if loadErr != nil {
			return loadErr
		}
		observed := s.clock.Now()
		if observed.Before(started) {
			return coordination.ErrClaimConflict
		}
		if checkErr := validateReadyClaimMaterials(p, current.TaskID, materials, observed); checkErr != nil {
			return checkErr
		}
		op := claimOperation{Kind: "dispatch", Token: req.Claim, RunID: req.RunID}
		operationDigest, encodeErr := op.digest()
		if encodeErr != nil {
			return encodeErr
		}
		next, fresh, changeErr := tx.mutateClaimInTransaction(tc, current.PlanID, current.TaskID, op, operationDigest)
		if changeErr != nil {
			return changeErr
		}
		if !fresh || next.At.Before(observed) {
			return coordination.ErrClaimConflict
		}
		working, changeErr := coordination.Advance(previous, coordination.AssignmentChange{AssignmentID: req.AssignmentID, ExpectedRevision: req.AssignmentRevision, HandoffDigest: h.Digest, Fence: h.Fence, State: coordination.AssignmentWorking, Actor: req.Actor, Reason: req.Reason}, next.At)
		if changeErr != nil {
			return changeErr
		}
		row, buildErr := assignmentRecord(working)
		if buildErr != nil {
			return buildErr
		}
		if insertErr := tx.driver.Insert(tc, row); insertErr != nil {
			return insertErr
		}
		admitted := coordination.TaskDispatch{RequestDigest: digest, Claim: next, Assignment: working, Outbox: coordination.TaskDispatchOutbox{EventID: id.New(coordination.PrefixTaskDispatchEvent), Type: "task_dispatched", ClaimID: next.ID, PlanID: next.PlanID, TaskID: next.TaskID, AssignmentID: req.AssignmentID, RunID: req.RunID}}
		if checkErr := admitted.Validate(req); checkErr != nil {
			return checkErr
		}
		retained := taskDispatchRecord{Request: req, Result: admitted}
		data, encodeErr := json.Marshal(retained)
		if encodeErr != nil {
			return encodeErr
		}
		if len(data) > 2<<20 {
			return coordination.ErrInvalidClaim
		}
		row, buildErr = record(kindReadyTaskDispatch, next.ID.String(), evidence.Digest(data), next.PlanID.String(), next.Scope, retained)
		if buildErr != nil {
			return buildErr
		}
		if insertErr := tx.driver.Insert(tc, row); insertErr != nil {
			return insertErr
		}
		finished := s.clock.Now()
		if finished.Before(next.At) || !finished.Before(next.ExpiresAt) || !beforeOptionalDeadline(finished, parent.Budget.Deadline) {
			return coordination.ErrClaimConflict
		}
		if checkErr := validateReadyClaimMaterials(p, next.TaskID, materials, finished); checkErr != nil {
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
		return coordination.TaskDispatch{}, err
	}
	return result, nil
}

func beforeOptionalDeadline(at, deadline time.Time) bool {
	return deadline.IsZero() || at.Before(deadline)
}

// GetTaskDispatch returns the original historical intent for recovery, never
// permission to invoke the reserved child or repeat external effects.
func (s *Repository) GetTaskDispatch(ctx context.Context, claimID id.ID) (coordination.TaskDispatch, error) {
	if claimID.IsNil() || claimID.Prefix() != coordination.PrefixClaim {
		return coordination.TaskDispatch{}, coordination.ErrInvalidClaim
	}
	retained, err := s.getTaskDispatch(ctx, claimID)
	if errors.Is(err, ErrAbsent) {
		err = coordination.ErrClaimNotFound
	}
	if err != nil {
		return coordination.TaskDispatch{}, err
	}
	return retained.Result, nil
}
