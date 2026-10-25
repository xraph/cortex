package assurance

import (
	"context"
	"time"

	"github.com/xraph/cortex/coordination"
)

var _ coordination.TaskUsageSnapshotStore = (*Repository)(nil)

// SnapshotTaskUsage reads the full verifier history and sealed journal under
// plan, run and journal guards. It never refunds capacity or closes job admission.
func (s *Repository) SnapshotTaskUsage(ctx context.Context, request coordination.TaskUsageRequest) (coordination.TaskUsageSnapshot, error) {
	req, err := request.Freeze()
	if err != nil {
		return coordination.TaskUsageSnapshot{}, err
	}
	if scopeErr := require(ctx, req.Seal.Request.Scope); scopeErr != nil {
		return coordination.TaskUsageSnapshot{}, scopeErr
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var snapshot coordination.TaskUsageSnapshot
	err = s.planTransaction(bounded, req.Claim.PlanID, func(tc context.Context, tx *Repository) error {
		snapshot = coordination.TaskUsageSnapshot{}
		claim, readErr := tx.GetTaskClaim(tc, req.Claim.PlanID, req.Claim.TaskID)
		if readErr != nil {
			return readErr
		}
		if claim.Token() != req.Claim || claim.RunID != req.Seal.Request.RunID || claim.Scope.Canonical() != req.Seal.Request.Scope.Canonical() || (claim.State != coordination.ClaimRecovering && claim.State != coordination.ClaimCompleted) {
			return coordination.ErrClaimConflict
		}
		locker, ok := tx.driver.(SubmissionRunReader)
		if !ok {
			return coordination.ErrAssignmentUnavailable
		}
		if _, readErr = locker.LockSubmissionRun(tc, claim.RunID); readErr != nil {
			return readErr
		}
		journal, readErr := tx.LockRunRecoverySnapshot(tc, req.Seal, req.SnapshotDigest)
		if readErr != nil {
			return readErr
		}
		rows, readErr := tx.driver.Select(tc, Query{Kind: KindJob, RunID: claim.RunID.String(), Limit: coordination.MaxTaskUsageJobs + 1})
		if readErr != nil {
			return readErr
		}
		if len(rows) > coordination.MaxTaskUsageJobs {
			return coordination.ErrInvalidTaskUsage
		}
		candidate := coordination.TaskUsageSnapshot{SchemaVersion: 1, Claim: claim, Journal: journal}
		bytes := 0
		for _, row := range rows {
			bytes += len(row.Body)
			if bytes > coordination.MaxTaskUsageBytes {
				return coordination.ErrInvalidTaskUsage
			}
			if recordErr := validateRecord(tc, row); recordErr != nil {
				return recordErr
			}
			job, decodeErr := decodeJob(row)
			if decodeErr != nil {
				return decodeErr
			}
			candidate.Jobs = append(candidate.Jobs, job)
		}
		snapshot, readErr = coordination.SealTaskUsageSnapshot(candidate)
		return readErr
	})
	if err != nil {
		return coordination.TaskUsageSnapshot{}, err
	}
	if contextErr := bounded.Err(); contextErr != nil {
		return coordination.TaskUsageSnapshot{}, contextErr
	}
	return snapshot, nil
}
