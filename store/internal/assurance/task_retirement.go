package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

const kindTaskRetirement = "task_retirement"

var _ coordination.TaskRetirementStore = (*Repository)(nil)

// GetTaskRetirement reads an immutable receipt for the original claim identity.
func (s *Repository) GetTaskRetirement(ctx context.Context, claimID id.ID) (*coordination.TaskRetirement, error) {
	if claimID.IsNil() || claimID.Prefix() != coordination.PrefixClaim {
		return nil, coordination.ErrInvalidClaim
	}
	row, err := s.get(ctx, Query{Kind: kindTaskRetirement, ID: claimID.String()})
	if errors.Is(err, ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var retained coordination.TaskRetirement
	if row.Deleted || len(row.Body) > 128<<10 || json.Unmarshal([]byte(row.Body), &retained) != nil || retained.Validate() != nil || row.Kind != kindTaskRetirement || row.ID != claimID.String() || retained.Request.Claim.ClaimID != claimID || row.RunID != retained.Claim.RunID.String() || row.ScopeCanon != retained.Claim.Scope.Canonical() || row.Digest != retained.Digest {
		return nil, coordination.ErrInvalidClaim
	}
	return &retained, nil
}

// RetireTaskClaim atomically checks closed unsuccessful work and its final
// accounting, then preserves the retired claim and receipt. Its caller must
// authenticate Recovery and fence permanent external quiescence throughout.
func (s *Repository) RetireTaskClaim(ctx context.Context, request coordination.TaskRetirementRequest) (coordination.TaskRetirement, error) {
	req, digest, err := request.Freeze()
	if err != nil {
		return coordination.TaskRetirement{}, err
	}
	if scopeErr := require(ctx, req.Recovery.Seal.Request.Scope); scopeErr != nil {
		return coordination.TaskRetirement{}, scopeErr
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result coordination.TaskRetirement
	err = s.planTransaction(bounded, req.Claim.PlanID, func(tc context.Context, tx *Repository) error {
		result = coordination.TaskRetirement{}
		retained, readErr := tx.GetTaskRetirement(tc, req.Claim.ClaimID)
		if readErr != nil {
			return readErr
		}
		if retained != nil {
			if retained.RequestDigest != digest {
				return coordination.ErrClaimConflict
			}
			previous, historyErr := tx.claimRevision(tc, req.Claim.PlanID, req.Claim.TaskID, req.Claim.Revision)
			if historyErr != nil {
				return historyErr
			}
			retired, historyErr := tx.claimRevision(tc, req.Claim.PlanID, req.Claim.TaskID, retained.Claim.Revision)
			if historyErr != nil {
				return historyErr
			}
			if retired.Digest != retained.Claim.Digest || coordination.ValidateClaimSuccessor(previous, retired) != nil {
				return coordination.ErrClaimConflict
			}
			result = *retained
			return nil
		}
		snapshot, readErr := tx.readTaskUsageSnapshot(tc, coordination.TaskUsageRequest{Claim: req.Claim, Seal: req.Recovery.Seal, SnapshotDigest: req.Recovery.SnapshotDigest})
		if readErr != nil {
			return readErr
		}
		claim := snapshot.Claim
		started := s.clock.Now()
		if _, retireErr := coordination.RetireRecoveredClaim(claim, req, started); retireErr != nil {
			return retireErr
		}
		if checkErr := tx.checkTaskRetirementAccounting(tc, req, snapshot); checkErr != nil {
			return checkErr
		}
		if checkErr := tx.checkTaskRetirementJournal(tc, req, snapshot); checkErr != nil {
			return checkErr
		}
		retired, retireErr := coordination.RetireRecoveredClaim(claim, req, s.clock.Now())
		if retireErr != nil {
			return retireErr
		}
		retainedResult, sealErr := coordination.SealTaskRetirement(coordination.TaskRetirement{SchemaVersion: 1, Request: req, RequestDigest: digest, Claim: retired})
		if sealErr != nil {
			return sealErr
		}
		claimRow, encodeErr := claimRecord(retired)
		if encodeErr != nil {
			return encodeErr
		}
		if insertErr := tx.driver.Insert(tc, claimRow); insertErr != nil {
			return insertErr
		}
		receiptRow, encodeErr := record(kindTaskRetirement, req.Claim.ClaimID.String(), retainedResult.Digest, retired.RunID.String(), retired.Scope, retainedResult)
		if encodeErr != nil {
			return encodeErr
		}
		if insertErr := tx.driver.Insert(tc, receiptRow); insertErr != nil {
			return insertErr
		}
		finished := s.clock.Now()
		if finished.Before(started) || finished.Before(retired.At) || !finished.Before(req.Recovery.ExpiresAt) {
			return coordination.ErrClaimConflict
		}
		result = retainedResult
		result.Fresh = true
		return nil
	})
	if err != nil {
		return coordination.TaskRetirement{}, err
	}
	if contextErr := bounded.Err(); contextErr != nil {
		return coordination.TaskRetirement{}, contextErr
	}
	return result, nil
}

func (s *Repository) checkTaskRetirementAccounting(ctx context.Context, req coordination.TaskRetirementRequest, snapshot coordination.TaskUsageSnapshot) error {
	closed, err := s.GetTaskUsageClosure(ctx, snapshot.Claim.RunID)
	if err != nil {
		return err
	}
	if closed == nil || closed.Digest != req.ClosureDigest || !closed.Request.MatchesClaim(snapshot.Claim) || (closed.Request.Termination == nil && closed.Request.Rejection == nil) {
		return coordination.ErrClaimConflict
	}
	if dispositionErr := s.checkTaskUsageDisposition(ctx, snapshot.Claim, closed.Request); dispositionErr != nil {
		return dispositionErr
	}
	settled, err := s.GetTaskUsageSettlement(ctx, snapshot.Claim.RunID)
	if err != nil {
		return err
	}
	if settled == nil || settled.Digest != req.SettlementDigest || settled.Request.ClosureDigest != closed.Digest || settled.Request.Usage.Claim != req.Claim || settled.Request.UsageDigest != snapshot.Digest || settled.Request.Usage.SnapshotDigest != snapshot.Journal.Digest || settled.Request.Usage.Seal.RequestDigest != req.Recovery.Seal.RequestDigest || !settled.Request.Usage.Seal.At.Equal(req.Recovery.Seal.At) || req.Recovery.ObservedAt.Before(settled.Settlement.At) {
		return coordination.ErrClaimConflict
	}
	binding, err := s.getClaimBudget(ctx, snapshot.Claim.ID)
	if err != nil {
		return err
	}
	original, current := binding.Result.Claim, snapshot.Claim
	if original.PlanID != current.PlanID || original.PlanDigest != current.PlanDigest || original.PlanRevision != current.PlanRevision || original.TaskID != current.TaskID || original.Worker != current.Worker || original.Fence != current.Fence || original.Scope.Canonical() != current.Scope.Canonical() || binding.Result.Reservation.Request.ID != settled.Request.ReservationID || binding.Result.Reservation.RequestDigest != settled.Request.ReservationDigest {
		return coordination.ErrClaimConflict
	}
	reservation, err := s.GetReservation(ctx, settled.Request.ReservationID)
	if err != nil {
		return err
	}
	if reservation.Validate() != nil || reservation.RequestDigest != settled.Request.ReservationDigest || reservation.Request.OwnerRef != current.ID.String() || reservation.Settlement == nil || reservation.Settlement.Digest != settled.Settlement.Digest || !reservation.Settlement.At.Equal(settled.Settlement.At) {
		return coordination.ErrClaimConflict
	}
	for _, job := range snapshot.Jobs {
		if job.State == verification.JobQueued || job.State == verification.JobRunning || job.State == verification.JobReady || req.Recovery.ObservedAt.Before(job.UpdatedAt) {
			return coordination.ErrClaimConflict
		}
	}
	return nil
}

func (s *Repository) checkTaskRetirementJournal(ctx context.Context, req coordination.TaskRetirementRequest, snapshot coordination.TaskUsageSnapshot) error {
	dispatch, err := s.taskDispatchClaim(ctx, snapshot.Claim)
	if err != nil {
		return err
	}
	reader, ok := s.driver.(SubmissionRunReader)
	if !ok {
		return coordination.ErrAssignmentUnavailable
	}
	current, err := reader.LockSubmissionRun(ctx, snapshot.Claim.RunID)
	if err != nil {
		return err
	}
	if current == nil || current.Provenance.TaskClaimID != snapshot.Claim.ID.String() || !evidence.ValidDigest(current.Provenance.PreparedDigest) || !evidence.ValidDigest(current.Provenance.AssignmentDigest) {
		return coordination.ErrClaimConflict
	}
	return validateRetirementCalls(req, snapshot, dispatch, current)
}

func validateRetirementCalls(req coordination.TaskRetirementRequest, snapshot coordination.TaskUsageSnapshot, dispatch coordination.Claim, current *run.Run) error {
	claim, journal, proof := snapshot.Claim, snapshot.Journal, req.Recovery
	terminalEffects := make(map[string]bool, len(journal.Effects))
	for _, effect := range journal.Effects {
		if effect.Request.RunCall == nil || effect.Receipt == nil || effect.Receipt.Validate(effect.Request) != nil {
			return coordination.ErrClaimConflict
		}
		terminalEffects[effect.Request.RunCall.Key()] = true
	}
	for _, entry := range journal.Calls {
		call := entry.Call
		r := call.Request
		if r.ClaimID != claim.ID || r.AgentID != current.AgentID || r.ParentRunID.String() != current.Provenance.ParentRunID || r.AssignmentDigest != current.Provenance.AssignmentDigest || r.PreparedDigest != current.Provenance.PreparedDigest || r.Deadline.After(claim.ExpiresAt) || call.StartedAt.Before(dispatch.At) || proof.ObservedAt.Before(call.StartedAt) || (!call.CompletedAt.IsZero() && proof.ObservedAt.Before(call.CompletedAt)) {
			return coordination.ErrClaimConflict
		}
		if r.Kind == execution.RunCallTool {
			if call.Completion == nil && !terminalEffects[r.Key()] {
				return coordination.ErrClaimConflict
			}
		} else if entry.Provider == nil || entry.Provider.ValidateObservation(call) != nil || proof.ObservedAt.Before(entry.Provider.ObservedAt) {
			return coordination.ErrClaimConflict
		}
	}
	return nil
}
