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
		var readErr error
		snapshot, readErr = tx.readTaskUsageSnapshot(tc, req)
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

// readTaskUsageSnapshot requires an existing plan transaction.
func (s *Repository) readTaskUsageSnapshot(ctx context.Context, req coordination.TaskUsageRequest) (coordination.TaskUsageSnapshot, error) {
	claim, readErr := s.GetTaskClaim(ctx, req.Claim.PlanID, req.Claim.TaskID)
	if readErr != nil {
		return coordination.TaskUsageSnapshot{}, readErr
	}
	if claim.Token() != req.Claim || claim.RunID != req.Seal.Request.RunID || claim.Scope.Canonical() != req.Seal.Request.Scope.Canonical() || (claim.State != coordination.ClaimRecovering && claim.State != coordination.ClaimCompleted) {
		return coordination.TaskUsageSnapshot{}, coordination.ErrClaimConflict
	}
	locker, ok := s.driver.(SubmissionRunReader)
	if !ok {
		return coordination.TaskUsageSnapshot{}, coordination.ErrAssignmentUnavailable
	}
	if _, readErr = locker.LockSubmissionRun(ctx, claim.RunID); readErr != nil {
		return coordination.TaskUsageSnapshot{}, readErr
	}
	journal, readErr := s.LockRunRecoverySnapshot(ctx, req.Seal, req.SnapshotDigest)
	if readErr != nil {
		return coordination.TaskUsageSnapshot{}, readErr
	}
	rows, readErr := s.driver.Select(ctx, Query{Kind: KindJob, RunID: claim.RunID.String(), Limit: coordination.MaxTaskUsageJobs + 1})
	if readErr != nil {
		return coordination.TaskUsageSnapshot{}, readErr
	}
	if len(rows) > coordination.MaxTaskUsageJobs {
		return coordination.TaskUsageSnapshot{}, coordination.ErrInvalidTaskUsage
	}
	candidate := coordination.TaskUsageSnapshot{SchemaVersion: 1, Claim: claim, Journal: journal}
	bytes := 0
	for _, row := range rows {
		bytes += len(row.Body)
		if bytes > coordination.MaxTaskUsageBytes {
			return coordination.TaskUsageSnapshot{}, coordination.ErrInvalidTaskUsage
		}
		if recordErr := validateRecord(ctx, row); recordErr != nil {
			return coordination.TaskUsageSnapshot{}, recordErr
		}
		job, decodeErr := decodeJob(row)
		if decodeErr != nil {
			return coordination.TaskUsageSnapshot{}, decodeErr
		}
		candidate.Jobs = append(candidate.Jobs, job)
	}
	return coordination.SealTaskUsageSnapshot(candidate)
}
