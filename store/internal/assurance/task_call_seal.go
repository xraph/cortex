package assurance

import (
	"context"
	"time"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

var _ coordination.TaskCallSealStore = (*Repository)(nil)

// SealTaskCalls closes ordinary accepted task admission under the plan, run and
// journal guards. The caller must hold current host authority through commit.
// Already admitted calls may still finish; no billing or quiescence is inferred.
func (s *Repository) SealTaskCalls(ctx context.Context, request coordination.TaskCallSealRequest) (execution.RunCallSeal, error) {
	req, _, err := request.Freeze()
	if err != nil {
		return execution.RunCallSeal{}, err
	}
	if scopeErr := require(ctx, req.Seal.Scope); scopeErr != nil {
		return execution.RunCallSeal{}, scopeErr
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if guardErr := s.ensureRunCallGuard(bounded, req.Seal.RunID); guardErr != nil {
		return execution.RunCallSeal{}, guardErr
	}
	var result execution.RunCallSeal
	err = s.planTransaction(bounded, req.Claim.PlanID, func(tc context.Context, tx *Repository) error {
		result = execution.RunCallSeal{}
		claim, accepted, readErr := tx.acceptedTaskClaim(tc, req.Claim)
		if readErr != nil {
			return readErr
		}
		if claim.Token() != req.Claim || claim.Digest != req.ClaimDigest || claim.State != coordination.ClaimCompleted || claim.RunID != req.Seal.RunID || claim.Scope.Canonical() != req.Seal.Scope.Canonical() || claim.AcceptanceID == nil {
			return coordination.ErrClaimConflict
		}
		if accepted.Request.Recovery != nil {
			return coordination.ErrClaimConflict
		}
		reader, ok := tx.driver.(SubmissionRunReader)
		if !ok {
			return coordination.ErrAssignmentUnavailable
		}
		current, readErr := reader.LockSubmissionRun(tc, claim.RunID)
		if readErr != nil {
			return readErr
		}
		receipt, readErr := tx.GetCommittedResult(tc, accepted.Request.Result.ResultID)
		if readErr != nil {
			return readErr
		}
		if current == nil || current.ID != claim.RunID || current.Scope.Canonical() != claim.Scope.Canonical() || current.State != run.StateCompleted || current.StopReason != run.StopFinalAnswer || current.VerificationStatus != verification.StatusPassed || current.ResultRef != accepted.Request.Result.ResultID.String() || current.Revision != receipt.RunRevision || receipt.Digest != accepted.Request.Result.ReceiptDigest {
			return coordination.ErrClaimConflict
		}
		locker, ok := tx.driver.(RunCallLocker)
		if !ok {
			return execution.ErrRunCallUnavailable
		}
		if lockErr := locker.LockRunCalls(tc, claim.RunID); lockErr != nil {
			return lockErr
		}
		if guardErr := tx.requireRunCallGuard(tc, claim.RunID); guardErr != nil {
			return guardErr
		}
		_, digest, freezeErr := req.Seal.Freeze()
		if freezeErr != nil {
			return freezeErr
		}
		result, readErr = tx.sealRunCallsInTransaction(tc, req.Seal, digest)
		return readErr
	})
	if err != nil {
		return execution.RunCallSeal{}, err
	}
	if contextErr := bounded.Err(); contextErr != nil {
		return execution.RunCallSeal{}, contextErr
	}
	return result, nil
}
