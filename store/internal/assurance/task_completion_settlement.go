package assurance

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

const kindTaskCompletionSettlement = "task_completion_settlement"

var _ coordination.TaskCompletionSettlementStore = (*Repository)(nil)

// GetTaskCompletionSettlement reads a retained terminal accounting observation.
func (s *Repository) GetTaskCompletionSettlement(ctx context.Context, assignmentID id.ID) (*coordination.TaskCompletionSettlement, error) {
	if assignmentID.IsNil() || assignmentID.Prefix() != coordination.PrefixAssignment {
		return nil, coordination.ErrInvalidReview
	}
	row, err := s.get(ctx, Query{Kind: kindTaskCompletionSettlement, ID: assignmentID.String()})
	if errors.Is(err, ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var receipt coordination.TaskCompletionSettlement
	if row.Deleted || len(row.Body) > 32<<10 || json.Unmarshal([]byte(row.Body), &receipt) != nil || receipt.Validate() != nil || receipt.AssignmentID != assignmentID || row.ID != assignmentID.String() || row.Kind != kindTaskCompletionSettlement || row.RunID != receipt.RunID.String() || row.Digest != receipt.Digest {
		return nil, coordination.ErrInvalidReview
	}
	return &receipt, nil
}

// SettleTaskCompletion closes an exact terminal reviewer without refunding
// unknown provider usage. Failed or malformed output is still charged. Absence
// or unfinished execution stays pending; this method never infers quiescence.
func (s *Repository) SettleTaskCompletion(ctx context.Context, assignmentID id.ID, completionDigest string) (*coordination.TaskCompletionSettlement, error) {
	if !evidence.ValidDigest(completionDigest) {
		return nil, coordination.ErrInvalidReview
	}
	completion, err := s.GetTaskCompletion(ctx, assignmentID)
	if err != nil {
		return nil, err
	}
	if completion == nil || completion.Digest != completionDigest {
		return nil, coordination.ErrReviewConflict
	}
	var result *coordination.TaskCompletionSettlement
	err = s.planTransaction(ctx, completion.Request.Handoff.PlanID, func(tc context.Context, tx *Repository) error {
		result = nil
		cached, readErr := tx.GetTaskCompletionSettlement(tc, assignmentID)
		if readErr != nil {
			return readErr
		}
		if cached != nil {
			if cached.CompletionDigest != completionDigest {
				return coordination.ErrReviewConflict
			}
			reservation, resErr := tx.GetReservation(tc, completion.Reservation.Request.ID)
			if resErr != nil {
				return resErr
			}
			if reservation.Settlement == nil || reservation.Settlement.Digest != cached.Settlement.Digest || !reservation.Settlement.At.Equal(cached.Settlement.At) {
				return coordination.ErrReviewConflict
			}
			result = cached
			return nil
		}
		reader, ok := tx.driver.(SubmissionRunReader)
		if !ok {
			return coordination.ErrAssignmentUnavailable
		}
		current, readErr := reader.LockSubmissionRun(tc, completion.Request.RunID)
		if errors.Is(readErr, cortex.ErrRunNotFound) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		req := completion.Request
		agentID, parseErr := id.ParseWithPrefix(req.Handoff.Receiver.Agent, id.PrefixAgent)
		if parseErr != nil {
			return coordination.ErrInvalidReview
		}
		if checkErr := validatePreparedProposalRun(preparedProposalBinding{RunID: req.RunID, SessionID: req.SessionID, AgentID: agentID, Scope: req.Handoff.Scope, PreparedDigest: req.PreparedDigest, InputDigest: req.InputDigest, NotBefore: completion.ObservedAt, Reservation: req.Reservation}, current); checkErr != nil {
			return checkErr
		}
		if current.Provenance.Purpose != run.PurposeEvaluation || current.Provenance.ParentRunID != req.Handoff.ParentRunID.String() || current.Provenance.AssignmentDigest != req.Handoff.Digest {
			return coordination.ErrInvalidReview
		}
		switch current.State {
		case run.StateCompleted, run.StateFailed, run.StateCancelled, run.StateIncomplete:
		default:
			return nil
		}
		now := s.clock.Now()
		if current.CompletedAt == nil || current.CompletedAt.Before(*current.StartedAt) || current.CompletedAt.After(now) {
			return coordination.ErrInvalidReview
		}
		data, marshalErr := json.Marshal(current)
		if marshalErr != nil {
			return marshalErr
		}
		minimum := preparedProposalUsage(current)
		settlement := execution.Settlement{ReservationID: completion.Reservation.Request.ID, RequestDigest: completion.Reservation.RequestDigest, Minimum: &minimum, Receipt: completion.Digest}
		frozen, digest, freezeErr := settlement.Freeze()
		if freezeErr != nil {
			return freezeErr
		}
		settled, settleErr := tx.settleInTransaction(tc, frozen, digest)
		if settleErr != nil {
			return settleErr
		}
		saved, sealErr := coordination.SealTaskCompletionSettlement(coordination.TaskCompletionSettlement{SchemaVersion: 1, AssignmentID: assignmentID, CompletionDigest: completionDigest, RunID: current.ID, RunRevision: current.Revision, RunDigest: evidence.Digest(data), Settlement: settled, ObservedAt: s.clock.Now()})
		if sealErr != nil {
			return sealErr
		}
		row, rowErr := record(kindTaskCompletionSettlement, assignmentID.String(), saved.Digest, current.ID.String(), req.Handoff.Scope, saved)
		if rowErr != nil {
			return rowErr
		}
		if writeErr := tx.driver.Insert(tc, row); writeErr != nil {
			return writeErr
		}
		result = &saved
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
