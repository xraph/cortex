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
	if row.Deleted || len(row.Body) > coordination.MaxHandoffBytes+128<<10 || json.Unmarshal([]byte(row.Body), &receipt) != nil || receipt.Validate() != nil || receipt.AssignmentID != assignmentID || row.ID != assignmentID.String() || row.Kind != kindTaskCompletionSettlement || row.RunID != receipt.RunID.String() || row.Digest != receipt.Digest {
		return nil, coordination.ErrInvalidReview
	}
	return &receipt, nil
}

// SettleTaskCompletion closes an exact terminal reviewer without refunding
// unknown provider usage. Failed or malformed output is still charged. Absence
// or unfinished execution stays pending; this method never infers quiescence.
func (s *Repository) SettleTaskCompletion(ctx context.Context, assignmentID id.ID, completionDigest string) (*coordination.TaskCompletionSettlement, error) {
	return s.settleTaskCompletion(ctx, assignmentID, completionDigest, nil)
}

// AbandonTaskCompletion records authenticated permanent quiescence. The caller
// must hold the same execution exclusion fence as every delayed reviewer.
func (s *Repository) AbandonTaskCompletion(ctx context.Context, request coordination.TaskCompletionAbandonmentRequest) (coordination.TaskCompletionSettlement, error) {
	req, _, err := request.Freeze()
	if err != nil {
		return coordination.TaskCompletionSettlement{}, err
	}
	if checkErr := require(ctx, req.Completion.Request.Handoff.Scope); checkErr != nil {
		return coordination.TaskCompletionSettlement{}, checkErr
	}
	result, err := s.settleTaskCompletion(ctx, req.Completion.Request.AssignmentID, req.Completion.Digest, &req)
	if err != nil {
		return coordination.TaskCompletionSettlement{}, err
	}
	if result == nil {
		return coordination.TaskCompletionSettlement{}, coordination.ErrReviewConflict
	}
	return *result, nil
}

func (s *Repository) settleTaskCompletion(ctx context.Context, assignmentID id.ID, completionDigest string, abandonment *coordination.TaskCompletionAbandonmentRequest) (*coordination.TaskCompletionSettlement, error) {
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
			if abandonment != nil {
				if cached.Abandonment == nil {
					return coordination.ErrReviewConflict
				}
				_, incoming, freezeErr := abandonment.Freeze()
				_, retained, retainedErr := cached.Abandonment.Freeze()
				if freezeErr != nil || retainedErr != nil || incoming != retained {
					return coordination.ErrReviewConflict
				}
			}
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
		if readErr != nil && !errors.Is(readErr, cortex.ErrRunNotFound) {
			return readErr
		}
		req := completion.Request
		if current != nil {
			agentID, parseErr := id.ParseWithPrefix(req.Handoff.Receiver.Agent, id.PrefixAgent)
			if parseErr != nil {
				return coordination.ErrInvalidReview
			}
			if checkErr := validatePreparedProposalRun(preparedProposalBinding{RunID: req.RunID, SessionID: req.SessionID, AgentID: agentID, Scope: req.Handoff.Scope, PreparedDigest: req.PreparedDigest, InputDigest: req.InputDigest, NotBefore: completion.ObservedAt, Reservation: req.Reservation}, current); checkErr != nil {
				return checkErr
			}
			if current.Provenance.ParentRunID != req.Handoff.ParentRunID.String() || current.Provenance.AssignmentDigest != req.Handoff.Digest {
				return coordination.ErrInvalidReview
			}
		}
		now := s.clock.Now()
		var minimum execution.Resources
		var runRevision int64
		var runDigest string
		if abandonment != nil {
			claim, claimErr := tx.GetTaskClaim(tc, req.Handoff.PlanID, req.Handoff.TaskID)
			if claimErr != nil {
				return claimErr
			}
			if claim.State == coordination.ClaimCompleted && claim.PlanDigest == req.Handoff.PlanDigest {
				return coordination.ErrReviewConflict
			}
			proof := abandonment.Proof
			if abandonment.Completion.Digest != completion.Digest || now.Before(proof.ObservedAt) || !now.Before(proof.ExpiresAt) || proof.ObservedAt.Before(completion.ObservedAt) || (current != nil && current.UpdatedAt.After(proof.ObservedAt)) {
				return coordination.ErrReviewConflict
			}
			if current != nil {
				minimum = preparedProposalUsage(current)
			}
		} else {
			if current == nil {
				return nil
			}
			switch current.State {
			case run.StateCompleted, run.StateFailed, run.StateCancelled, run.StateIncomplete:
			default:
				return nil
			}
			if current.CompletedAt == nil || current.CompletedAt.Before(*current.StartedAt) || current.CompletedAt.After(now) {
				return coordination.ErrInvalidReview
			}
			data, marshalErr := json.Marshal(current)
			if marshalErr != nil {
				return marshalErr
			}
			minimum = preparedProposalUsage(current)
			runRevision, runDigest = current.Revision, evidence.Digest(data)
		}
		settlement := execution.Settlement{ReservationID: completion.Reservation.Request.ID, RequestDigest: completion.Reservation.RequestDigest, Minimum: &minimum, Receipt: completion.Digest}
		frozen, digest, freezeErr := settlement.Freeze()
		if freezeErr != nil {
			return freezeErr
		}
		settled, settleErr := tx.settleInTransaction(tc, frozen, digest)
		if settleErr != nil {
			return settleErr
		}
		saved, sealErr := coordination.SealTaskCompletionSettlement(coordination.TaskCompletionSettlement{SchemaVersion: 1, AssignmentID: assignmentID, CompletionDigest: completionDigest, RunID: req.RunID, RunRevision: runRevision, RunDigest: runDigest, Abandonment: abandonment, Settlement: settled, ObservedAt: s.clock.Now()})
		if sealErr != nil {
			return sealErr
		}
		row, rowErr := record(kindTaskCompletionSettlement, assignmentID.String(), saved.Digest, req.RunID.String(), req.Handoff.Scope, saved)
		if rowErr != nil {
			return rowErr
		}
		if writeErr := tx.driver.Insert(tc, row); writeErr != nil {
			return writeErr
		}
		if abandonment != nil && !s.clock.Now().Before(abandonment.Proof.ExpiresAt) {
			return coordination.ErrReviewConflict
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
