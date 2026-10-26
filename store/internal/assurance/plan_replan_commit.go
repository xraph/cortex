package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/verification"
)

type planReplanCommit struct {
	ControlDigest string        `json:"control_digest"`
	OutcomeDigest string        `json:"outcome_digest"`
	Plan          planning.Plan `json:"plan"`
}

// CommitPlanReplan appends a validated proposal only after every dispatched
// ownership incarnation is permanently quiescent and its journal and accounting
// agree. It retains previous claims, artifacts, results and effect debt.
func (s *Repository) CommitPlanReplan(ctx context.Context, control coordination.PlanReplanControl) (planning.Plan, error) {
	req, digest, err := control.Freeze()
	if err != nil {
		return planning.Plan{}, err
	}
	if req.Operation != "commit" {
		return planning.Plan{}, planning.ErrInvalid
	}
	if checkErr := require(ctx, req.Request.Progress.Plan.Scope); checkErr != nil {
		return planning.Plan{}, checkErr
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result planning.Plan
	err = s.planTransaction(bounded, req.Request.Progress.Plan.ID, func(tc context.Context, tx *Repository) error {
		result = planning.Plan{}
		admission, readErr := tx.requireReplan(tc, req.Request)
		if readErr != nil {
			return readErr
		}
		outcome, readErr := tx.GetPlanReplanOutcome(tc, req.Request.ID)
		if readErr != nil {
			return readErr
		}
		if outcome == nil || outcome.Proposed == nil || outcome.AdmissionDigest != admission.RequestDigest {
			return planning.ErrConflict
		}
		row, readErr := tx.get(tc, Query{Kind: kindPlanReplanCommit, ID: req.Request.ID.String()})
		if readErr == nil {
			var retained planReplanCommit
			if row.Deleted || len(row.Body) > 2<<20 || json.Unmarshal([]byte(row.Body), &retained) != nil || retained.ControlDigest != digest || row.Digest != digest || retained.OutcomeDigest != outcome.Digest || retained.Plan.Digest != outcome.Proposed.Digest || planning.Validate(retained.Plan, planning.Limits{}) != nil {
				return planning.ErrConflict
			}
			result = retained.Plan
			return nil
		}
		if !errors.Is(readErr, ErrAbsent) {
			return readErr
		}
		current, readErr := tx.GetPlan(tc, req.Request.Progress.Plan.ID)
		if readErr != nil {
			return readErr
		}
		if current.Digest != req.Request.Progress.Plan.Digest || !s.clock.Now().Before(current.Budget.Deadline) {
			return planning.ErrConflict
		}
		if checkErr := tx.checkReplanOwnership(tc, req, s.clock.Now()); checkErr != nil {
			return checkErr
		}
		next, commitErr := tx.commitRevision(tc, planning.Revision{Plan: *outcome.Proposed, Actor: req.Request.Actor, Reason: req.Request.Reason}, current.Revision)
		if commitErr != nil {
			return commitErr
		}
		if next.Digest != outcome.Proposed.Digest {
			return planning.ErrConflict
		}
		retained := planReplanCommit{ControlDigest: digest, OutcomeDigest: outcome.Digest, Plan: next}
		recorded, encodeErr := record(kindPlanReplanCommit, req.Request.ID.String(), digest, current.ID.String(), current.Scope, retained)
		if encodeErr != nil {
			return encodeErr
		}
		if insertErr := tx.driver.Insert(tc, recorded); insertErr != nil {
			return insertErr
		}
		now := s.clock.Now()
		if !now.Before(current.Budget.Deadline) {
			return planning.ErrConflict
		}
		for _, proof := range req.Claims {
			if !now.Before(proof.ExpiresAt) {
				return planning.ErrConflict
			}
		}
		result = next
		return nil
	})
	if err != nil {
		return planning.Plan{}, err
	}
	if checkErr := bounded.Err(); checkErr != nil {
		return planning.Plan{}, checkErr
	}
	return result, nil
}

func (s *Repository) checkReplanOwnership(ctx context.Context, req coordination.PlanReplanControl, now time.Time) error {
	proofs := map[string]coordination.TaskRecoveryProof{}
	for _, proof := range req.Claims {
		proofs[proof.ClaimDigest] = proof
	}
	count := 0
	var after id.ID
	for {
		claims, err := s.ListTaskClaims(ctx, coordination.ClaimListRequest{PlanID: req.Request.Progress.Plan.ID, AfterTaskID: after, Limit: 256})
		if err != nil {
			return err
		}
		for _, claim := range claims {
			if claim.State == coordination.ClaimReleased {
				continue
			}
			if claim.State != coordination.ClaimCompleted && claim.State != coordination.ClaimRetired {
				return planning.ErrConflict
			}
			proof, ok := proofs[claim.Digest]
			if !ok || proof.ObservedAt.Before(claim.At) || now.Before(proof.ObservedAt) || !now.Before(proof.ExpiresAt) || proof.Seal.Request.RunID != claim.RunID || proof.Seal.Request.Scope.Canonical() != claim.Scope.Canonical() {
				return planning.ErrConflict
			}
			count++
			if checkErr := s.checkReplanClaim(ctx, claim, proof); checkErr != nil {
				return checkErr
			}
		}
		if len(claims) < 256 {
			break
		}
		after = claims[len(claims)-1].TaskID
		if count >= planning.MaxProgressAttempts {
			return planning.ErrConflict
		}
	}
	if count != len(proofs) {
		return planning.ErrConflict
	}
	return nil
}
func (s *Repository) checkReplanClaim(ctx context.Context, claim coordination.Claim, proof coordination.TaskRecoveryProof) error {
	reader, ok := s.driver.(SubmissionRunReader)
	if !ok {
		return coordination.ErrAssignmentUnavailable
	}
	current, err := reader.LockSubmissionRun(ctx, claim.RunID)
	if err != nil {
		return err
	}
	if current == nil {
		return planning.ErrConflict
	}
	journal, err := s.LockRunRecoverySnapshot(ctx, proof.Seal, proof.SnapshotDigest)
	if err != nil {
		return err
	}
	settled, err := s.GetTaskUsageSettlement(ctx, claim.RunID)
	if err != nil {
		return err
	}
	closed, err := s.GetTaskUsageClosure(ctx, claim.RunID)
	if err != nil {
		return err
	}
	if settled == nil || closed == nil || settled.Request.Usage.Claim.ClaimID != claim.ID || settled.Request.Usage.SnapshotDigest != journal.Digest || settled.Request.ClosureDigest != closed.Digest || closed.Request.Usage.Claim.ClaimID != claim.ID || proof.ObservedAt.Before(settled.Settlement.At) {
		return planning.ErrConflict
	}
	reserved, err := s.GetReservation(ctx, settled.Request.ReservationID)
	if err != nil {
		return err
	}
	if reserved.Settlement == nil || reserved.Settlement.Digest != settled.Settlement.Digest || reserved.Request.OwnerRef != claim.ID.String() {
		return planning.ErrConflict
	}
	if claim.State == coordination.ClaimRetired {
		retired, readErr := s.GetTaskRetirement(ctx, claim.ID)
		if readErr != nil {
			return readErr
		}
		if retired == nil || retired.Claim.Digest != claim.Digest || retired.Request.SettlementDigest != settled.Digest {
			return planning.ErrConflict
		}
	}
	closureClaim, err := s.claimRevision(ctx, claim.PlanID, claim.TaskID, closed.Request.Usage.Claim.Revision)
	if err != nil {
		return err
	}
	if !closed.Request.MatchesClaim(closureClaim) {
		return planning.ErrConflict
	}
	if checkErr := s.checkTaskUsageDisposition(ctx, closureClaim, closed.Request); checkErr != nil {
		return checkErr
	}
	dispatch, err := s.taskDispatchClaim(ctx, claim)
	if err != nil {
		return err
	}
	snapshot := coordination.TaskUsageSnapshot{Claim: claim, Journal: journal}
	if checkErr := validateRetirementCalls(coordination.TaskRetirementRequest{Recovery: proof}, snapshot, dispatch, current); checkErr != nil {
		return checkErr
	}
	rows, err := s.driver.Select(ctx, Query{Kind: KindJob, RunID: claim.RunID.String(), Limit: coordination.MaxTaskUsageJobs + 1})
	if err != nil {
		return err
	}
	if len(rows) > coordination.MaxTaskUsageJobs {
		return planning.ErrConflict
	}
	for _, row := range rows {
		if checkErr := validateRecord(ctx, row); checkErr != nil {
			return checkErr
		}
		job, decodeErr := decodeJob(row)
		if decodeErr != nil {
			return decodeErr
		}
		if job.State == verification.JobQueued || job.State == verification.JobRunning || job.State == verification.JobReady || proof.ObservedAt.Before(job.UpdatedAt) {
			return planning.ErrConflict
		}
	}
	return nil
}

// GetPlanReplanCommit reads an acknowledged historical graph transition.
func (s *Repository) GetPlanReplanCommit(ctx context.Context, key id.ID) (*planning.Plan, error) {
	if key.IsNil() || key.Prefix() != coordination.PrefixPlanReplan {
		return nil, planning.ErrInvalid
	}
	row, err := s.get(ctx, Query{Kind: kindPlanReplanCommit, ID: key.String()})
	if errors.Is(err, ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var retained planReplanCommit
	if row.Deleted || len(row.Body) > 2<<20 || json.Unmarshal([]byte(row.Body), &retained) != nil || row.Digest != retained.ControlDigest || planning.Validate(retained.Plan, planning.Limits{}) != nil || row.RunID != retained.Plan.ID.String() || row.ScopeCanon != retained.Plan.Scope.Canonical() {
		return nil, planning.ErrInvalid
	}
	outcome, err := s.GetPlanReplanOutcome(ctx, key)
	if err != nil {
		return nil, err
	}
	if outcome == nil || outcome.Proposed == nil || outcome.Digest != retained.OutcomeDigest || outcome.Proposed.Digest != retained.Plan.Digest {
		return nil, planning.ErrConflict
	}
	return &retained.Plan, nil
}
