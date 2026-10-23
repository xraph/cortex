package assurance

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

var _ verification.ResultSnapshotStore = (*Repository)(nil)

// ReadResultSnapshot reads current result material under the backend's run fence.
// It supplies no public authorization and never updates lifecycle state or events.
func (s *Repository) ReadResultSnapshot(ctx context.Context, request verification.ResultReadRequest) (verification.ResultSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return verification.ResultSnapshot{}, err
	}
	if err := request.Validate(); err != nil {
		return verification.ResultSnapshot{}, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return verification.ResultSnapshot{}, err
	}
	driver, ok := s.driver.(RunDriver)
	if !ok {
		return verification.ResultSnapshot{}, verification.ErrCommitUnavailable
	}
	var snapshot verification.ResultSnapshot
	err = driver.TransactRun(ctx, request.RunID, func(txctx context.Context, tx RunTransaction) error {
		snapshot = verification.ResultSnapshot{}
		repository := s.inTransaction(tx)
		receipt, readErr := repository.GetCommittedResult(txctx, request.ResultID)
		if readErr != nil {
			return readErr
		}
		binding := receipt.Request.Current
		if receipt.Digest != request.ReceiptDigest || binding.RunID != request.RunID || binding.Scope.Canonical() != scope.Canonical() {
			return verification.ErrConflict
		}
		current, readErr := tx.GetRun(txctx, request.RunID)
		if readErr != nil {
			return readErr
		}
		if current.Scope.Canonical() != scope.Canonical() || current.ID != request.RunID || current.Revision != receipt.RunRevision || current.State != run.StateCompleted || current.StopReason != run.StopFinalAnswer || current.VerificationStatus != verification.StatusPassed || current.ResultRef != request.ResultID.String() || current.Provenance.PreparedDigest != binding.Context.PreparedDigest || current.Provenance.AcceptanceRef != receipt.Request.ContractID.String() {
			return cortex.ErrRunConflict
		}
		if inputErr := repository.validateExecutionInputs(txctx, current, binding); inputErr != nil {
			return inputErr
		}
		material, readErr := repository.loadResultMaterial(txctx, receipt.Request)
		if readErr != nil {
			return readErr
		}
		if current.Output != material.output {
			return cortex.ErrRunConflict
		}
		candidate := material.snapshot(receipt, s.clock.Now().UTC())
		if validateErr := candidate.Validate(request, candidate.ObservedAt); validateErr != nil {
			return validateErr
		}
		snapshot = candidate
		return txctx.Err()
	})
	if err != nil {
		return verification.ResultSnapshot{}, err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return verification.ResultSnapshot{}, contextErr
	}
	// The transaction can wait for its final acknowledgement. Expiry during that
	// wait must not return content that was fresh only inside the callback.
	if validateErr := snapshot.Validate(request, s.clock.Now().UTC()); validateErr != nil {
		return verification.ResultSnapshot{}, validateErr
	}
	return snapshot, nil
}
