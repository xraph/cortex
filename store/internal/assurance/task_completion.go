package assurance

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

const kindTaskCompletion = "task_completion"

var _ coordination.TaskCompletionStore = (*Repository)(nil)

// GetTaskCompletion returns the original reserved review, or nil if absent.
func (s *Repository) GetTaskCompletion(ctx context.Context, assignmentID id.ID) (*coordination.TaskCompletion, error) {
	if assignmentID.IsNil() || assignmentID.Prefix() != coordination.PrefixAssignment {
		return nil, coordination.ErrInvalidReview
	}
	row, err := s.get(ctx, Query{Kind: kindTaskCompletion, ID: assignmentID.String()})
	if errors.Is(err, ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result coordination.TaskCompletion
	if row.Deleted || len(row.Body) > coordination.MaxHandoffBytes+64<<10 || json.Unmarshal([]byte(row.Body), &result) != nil || result.Validate() != nil || row.ID != result.Request.AssignmentID.String() || row.Kind != kindTaskCompletion || row.RunID != result.Request.Handoff.PlanID.String() || row.Digest != result.Digest || row.ScopeCanon != result.Request.Handoff.Scope.Canonical() {
		return nil, coordination.ErrInvalidReview
	}
	return &result, nil
}

// ReserveTaskCompletion atomically binds reviewer identity, progress and shared
// allocation. A retry never returns fresh permission, even after a lost reply.
func (s *Repository) ReserveTaskCompletion(ctx context.Context, request coordination.TaskCompletionRequest) (coordination.TaskCompletion, error) {
	req, digest, err := request.Freeze()
	if err != nil {
		return coordination.TaskCompletion{}, err
	}
	if scopeErr := require(ctx, req.Handoff.Scope); scopeErr != nil {
		return coordination.TaskCompletion{}, scopeErr
	}
	var result coordination.TaskCompletion
	err = s.planTransaction(ctx, req.Handoff.PlanID, func(tc context.Context, tx *Repository) error {
		result = coordination.TaskCompletion{}
		cached, readErr := tx.GetTaskCompletion(tc, req.AssignmentID)
		if readErr != nil {
			return readErr
		}
		if cached != nil {
			if cached.RequestDigest != digest {
				return coordination.ErrReviewConflict
			}
			result = *cached
			return nil
		}
		p, readErr := tx.GetPlan(tc, req.Handoff.PlanID)
		if readErr != nil {
			return readErr
		}
		subject, readErr := tx.GetAssignment(tc, req.AssignmentID)
		if readErr != nil {
			return readErr
		}
		if subject.Submission == nil {
			return coordination.ErrInvalidReview
		}
		claim, readErr := tx.GetTaskClaim(tc, p.ID, subject.Handoff.TaskID)
		if readErr != nil {
			return readErr
		}
		h := subject.Handoff
		if (claim.State != coordination.ClaimDispatching && claim.State != coordination.ClaimRecovering) || claim.PlanDigest != p.Digest || claim.PlanRevision != p.Revision || claim.RunID != subject.Submission.RunID || claim.Scope.Canonical() != h.Scope.Canonical() || claim.Worker != h.Receiver || claim.Fence != h.Fence || claim.GrantRef != h.GrantRef {
			return coordination.ErrClaimConflict
		}
		job, readErr := tx.GetVerificationJob(tc, subject.Submission.JobID)
		if readErr != nil {
			return readErr
		}
		if job.State != verification.JobCommitted {
			return coordination.ErrInvalidReview
		}
		reader, ok := tx.driver.(SubmissionRunReader)
		if !ok {
			return coordination.ErrAssignmentUnavailable
		}
		current, readErr := reader.LockSubmissionRun(tc, subject.Submission.RunID)
		if readErr != nil {
			return readErr
		}
		observedSubject := subject
		pins := *subject.Submission
		pins.RunRevision = current.Revision
		observedSubject.Submission = &pins
		if checkErr := tx.validateAssignmentSubmission(tc, observedSubject, s.clock.Now()); checkErr != nil {
			return checkErr
		}
		if checkErr := coordination.ValidateTaskCompletion(req, subject, job.Request.Binding, p, s.clock.Now()); checkErr != nil {
			return checkErr
		}
		// An existing reviewer identity may already have run. Never adopt it as a new
		// dispatch, even if its metadata happens to match this request.
		if _, readErr = tx.GetAssignment(tc, req.Handoff.AssignmentID); !errors.Is(readErr, coordination.ErrAssignmentNotFound) {
			if readErr != nil {
				return readErr
			}
			return coordination.ErrReviewConflict
		}
		_, reservationDigest, freezeErr := req.Reservation.Freeze()
		if freezeErr != nil {
			return freezeErr
		}
		reservation, fresh, reserveErr := tx.reserveInTransaction(tc, req.Reservation, reservationDigest)
		if reserveErr != nil {
			return reserveErr
		}
		if !fresh {
			return coordination.ErrReviewConflict
		}
		observed := s.clock.Now()
		assigned, buildErr := coordination.Propose(req.Handoff, observed)
		if buildErr != nil {
			return buildErr
		}
		for _, state := range []coordination.AssignmentState{coordination.AssignmentProposed, coordination.AssignmentAccepted, coordination.AssignmentWorking} {
			if state != coordination.AssignmentProposed {
				assigned, buildErr = coordination.Advance(assigned, coordination.AssignmentChange{AssignmentID: req.Handoff.AssignmentID, ExpectedRevision: assigned.Revision, HandoffDigest: req.Handoff.Digest, Fence: req.Handoff.Fence, State: state, Actor: req.Handoff.Receiver.Principal, Reason: req.Reason}, observed)
				if buildErr != nil {
					return buildErr
				}
			}
			row, rowErr := assignmentRecord(assigned)
			if rowErr != nil {
				return rowErr
			}
			if writeErr := tx.driver.Insert(tc, row); writeErr != nil {
				return writeErr
			}
		}
		saved, sealErr := coordination.SealTaskCompletion(coordination.TaskCompletion{SchemaVersion: 1, Request: req, RequestDigest: digest, Reservation: reservation, ObservedAt: observed})
		if sealErr != nil {
			return sealErr
		}
		row, rowErr := record(kindTaskCompletion, req.AssignmentID.String(), saved.Digest, p.ID.String(), p.Scope, saved)
		if rowErr != nil {
			return rowErr
		}
		if writeErr := tx.driver.Insert(tc, row); writeErr != nil {
			return writeErr
		}
		if checkErr := coordination.ValidateTaskCompletion(req, subject, job.Request.Binding, p, s.clock.Now()); checkErr != nil {
			return checkErr
		}
		result = saved
		result.Fresh = true
		return nil
	})
	if errors.Is(err, ErrExists) {
		err = coordination.ErrReviewConflict
	}
	if err != nil {
		return coordination.TaskCompletion{}, err
	}
	if err := ctx.Err(); err != nil {
		return coordination.TaskCompletion{}, err
	}
	return result, nil
}
