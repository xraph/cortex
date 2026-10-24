package assurance

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

const kindTaskExecutionRenewal = "task_execution_renewal"

var _ coordination.TaskRenewalStore = (*Repository)(nil)

type taskRenewalRecord struct {
	Request coordination.TaskRenewalRequest `json:"request"`
	Result  coordination.TaskRenewal        `json:"result"`
}

func (s *Repository) taskRenewal(ctx context.Context, digest string) (coordination.TaskRenewal, error) {
	row, err := s.get(ctx, Query{Kind: kindTaskExecutionRenewal, ID: digest})
	if err != nil {
		return coordination.TaskRenewal{}, err
	}
	var retained taskRenewalRecord
	if row.Deleted || len(row.Body) > 2<<20 || json.Unmarshal([]byte(row.Body), &retained) != nil || retained.Result.Validate(retained.Request) != nil || row.Kind != kindTaskExecutionRenewal || row.ID != digest || retained.Result.RequestDigest != digest || row.RunID != retained.Request.Claim.PlanID.String() || row.ScopeCanon != retained.Request.Claim.Scope.Canonical() || row.Digest != evidence.Digest([]byte(row.Body)) {
		return coordination.TaskRenewal{}, coordination.ErrInvalidClaim
	}
	previous, err := s.claimRevision(ctx, retained.Request.Claim.PlanID, retained.Request.Claim.TaskID, retained.Request.Claim.Revision)
	if err != nil {
		return coordination.TaskRenewal{}, err
	}
	next, err := s.claimRevision(ctx, previous.PlanID, previous.TaskID, previous.Revision+1)
	if err != nil {
		return coordination.TaskRenewal{}, err
	}
	if previous.Digest != retained.Request.Claim.Digest || next.Digest != retained.Result.Claim.Digest {
		return coordination.TaskRenewal{}, coordination.ErrInvalidClaim
	}
	return retained.Result, nil
}

// RenewTaskExecution extends ownership only while its retained execution chain
// remains eligible. The plan transaction fences claims, assignment, parent,
// child, prerequisites and shared budget. No lock spans an external invocation.
func (s *Repository) RenewTaskExecution(ctx context.Context, request coordination.TaskRenewalRequest) (coordination.TaskRenewal, error) {
	if err := require(ctx, request.Claim.Scope); err != nil {
		return coordination.TaskRenewal{}, err
	}
	req, digest, err := request.Freeze()
	if err != nil {
		return coordination.TaskRenewal{}, err
	}
	var result coordination.TaskRenewal
	err = s.planTransaction(ctx, req.Claim.PlanID, func(tc context.Context, tx *Repository) error {
		result = coordination.TaskRenewal{}
		cached, readErr := tx.taskRenewal(tc, digest)
		if readErr == nil {
			result = cached
			return nil
		}
		if !errors.Is(readErr, ErrAbsent) {
			return readErr
		}
		current, readErr := tx.GetTaskClaim(tc, req.Claim.PlanID, req.Claim.TaskID)
		if readErr != nil {
			return readErr
		}
		if current.Digest != req.Claim.Digest {
			return coordination.ErrClaimConflict
		}
		p, readErr := tx.GetPlan(tc, current.PlanID)
		if readErr != nil {
			return readErr
		}
		if p.Digest != current.PlanDigest || p.Revision != current.PlanRevision {
			return coordination.ErrClaimConflict
		}
		start, readErr := tx.getTaskRunStart(tc, current.ID)
		if readErr != nil {
			return readErr
		}
		if start.Result.RunID != current.RunID {
			return coordination.ErrClaimConflict
		}
		assignment, readErr := tx.GetAssignment(tc, req.AssignmentID)
		if readErr != nil {
			return readErr
		}
		h := assignment.Handoff
		if (assignment.State != coordination.AssignmentWorking && assignment.State != coordination.AssignmentBlocked && assignment.State != coordination.AssignmentSubmitted) || assignment.Revision != req.AssignmentRevision || assignment.Digest != req.AssignmentDigest || h.ParentRunID != req.ParentRunID || h.PlanID != current.PlanID || h.PlanRevision != current.PlanRevision || h.PlanDigest != current.PlanDigest || h.TaskID != current.TaskID || h.Scope.Canonical() != current.Scope.Canonical() || h.Receiver != current.Worker || h.Fence != current.Fence || h.GrantRef != current.GrantRef || h.Purpose != coordination.HandoffWork {
			return coordination.ErrClaimConflict
		}
		parent, readErr := tx.lockTaskParent(tc, req.ParentRunID, req.ParentRunRevision, current.Scope)
		if readErr != nil {
			return readErr
		}
		reader, ok := tx.driver.(SubmissionRunReader)
		if !ok {
			return coordination.ErrAssignmentUnavailable
		}
		child, readErr := reader.LockSubmissionRun(tc, current.RunID)
		if readErr != nil {
			return readErr
		}
		if child == nil || child.ID != current.RunID || child.Revision != req.RunRevision || child.Scope.Canonical() != current.Scope.Canonical() || child.AgentID.String() != current.Worker.Agent || (child.State != run.StateRunning && child.State != run.StatePaused && child.State != run.StateCompleted) || child.Provenance.TaskClaimID != current.ID.String() || child.Provenance.ParentRunID != req.ParentRunID.String() || child.Provenance.AssignmentDigest != h.Digest || !child.Provenance.VerificationRequired {
			return coordination.ErrClaimConflict
		}
		intent, readErr := tx.GetRunCaptureIntent(tc, current.RunID)
		if readErr != nil {
			return readErr
		}
		binding := intent.Template.Binding
		if child.Provenance.PreparedDigest != binding.Context.PreparedDigest || child.Provenance.VerificationIntentID != intent.ID.String() || child.Provenance.AcceptanceRef != intent.Template.ContractID.String() || binding.Context.ExecutionInputs == nil || child.Provenance.ExecutionInputsDigest != binding.Context.ExecutionInputs.SHA256 || binding.Context.PrincipalRef != current.Worker.Principal {
			return coordination.ErrClaimConflict
		}
		at := s.clock.Now()
		if checkErr := coordination.ValidateHandoffPlan(h, p, at); checkErr != nil {
			return checkErr
		}
		materials, loadErr := tx.acceptedPrerequisiteMaterials(tc, p, current.TaskID, req.Dependencies, 32<<20)
		if loadErr != nil {
			return loadErr
		}
		observed := s.clock.Now()
		if observed.Before(at) {
			return coordination.ErrClaimConflict
		}
		if checkErr := validateReadyClaimMaterials(p, current.TaskID, materials, observed); checkErr != nil {
			return checkErr
		}
		op := claimOperation{Kind: "renew", Token: current.Token(), Duration: req.Duration}
		operationDigest, encodeErr := op.digest()
		if encodeErr != nil {
			return encodeErr
		}
		next, fresh, renewErr := tx.mutateClaimInTransaction(tc, current.PlanID, current.TaskID, op, operationDigest)
		if renewErr != nil {
			return renewErr
		}
		if !fresh || next.At.Before(observed) || child.Budget.Deadline.IsZero() || next.ExpiresAt.After(child.Budget.Deadline) || next.ExpiresAt.After(intent.Template.Deadline) || next.ExpiresAt.After(h.Deadline) || (!parent.Budget.Deadline.IsZero() && next.ExpiresAt.After(parent.Budget.Deadline)) {
			return coordination.ErrClaimConflict
		}
		renewed := coordination.TaskRenewal{RequestDigest: digest, Claim: next, Outbox: coordination.TaskRenewalOutbox{EventID: id.New(coordination.PrefixTaskRenewalEvent), Type: "task_renewed", ClaimID: next.ID, PlanID: next.PlanID, TaskID: next.TaskID, RunID: next.RunID}}
		if checkErr := renewed.Validate(req); checkErr != nil {
			return checkErr
		}
		retained := taskRenewalRecord{Request: req, Result: renewed}
		data, encodeErr := json.Marshal(retained)
		if encodeErr != nil || len(data) > 2<<20 {
			return coordination.ErrInvalidClaim
		}
		row, buildErr := record(kindTaskExecutionRenewal, digest, evidence.Digest(data), next.PlanID.String(), next.Scope, retained)
		if buildErr != nil {
			return buildErr
		}
		if insertErr := tx.driver.Insert(tc, row); insertErr != nil {
			return insertErr
		}
		finished := s.clock.Now()
		if finished.Before(next.At) || !finished.Before(next.ExpiresAt) {
			return coordination.ErrClaimConflict
		}
		if checkErr := validateReadyClaimMaterials(p, current.TaskID, materials, finished); checkErr != nil {
			return checkErr
		}
		result = renewed
		result.Fresh = true
		return nil
	})
	if errors.Is(err, ErrExists) {
		err = coordination.ErrClaimConflict
	}
	if err != nil {
		return coordination.TaskRenewal{}, err
	}
	return result, nil
}
