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
	_, err := cortex.RequireScope(ctx)
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
		current, readErr := tx.GetRun(txctx, request.RunID)
		if readErr != nil {
			return readErr
		}
		var snapshotErr error
		snapshot, _, snapshotErr = repository.resultSnapshotForRun(txctx, request, current, 8<<20)
		if snapshotErr != nil {
			return snapshotErr
		}

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

// resultSnapshotForRun uses a run already fenced by the caller's transaction.
func (s *Repository) resultSnapshotForRun(ctx context.Context, request verification.ResultReadRequest, current *run.Run, limit int) (verification.ResultSnapshot, int, error) {
	receipt, err := s.GetCommittedResult(ctx, request.ResultID)
	if err != nil {
		return verification.ResultSnapshot{}, 0, err
	}
	binding := receipt.Request.Current
	if request.Validate() != nil || receipt.Digest != request.ReceiptDigest || binding.RunID != request.RunID || require(ctx, binding.Scope) != nil {
		return verification.ResultSnapshot{}, 0, verification.ErrConflict
	}
	if current == nil || current.Scope.Canonical() != binding.Scope.Canonical() || current.ID != request.RunID || current.Revision != receipt.RunRevision || current.State != run.StateCompleted || current.StopReason != run.StopFinalAnswer || current.VerificationStatus != verification.StatusPassed || current.ResultRef != request.ResultID.String() || current.Provenance.PreparedDigest != binding.Context.PreparedDigest || current.Provenance.AcceptanceRef != receipt.Request.ContractID.String() {
		return verification.ResultSnapshot{}, 0, cortex.ErrRunConflict
	}
	if checkErr := s.validateExecutionInputs(ctx, current, binding); checkErr != nil {
		return verification.ResultSnapshot{}, 0, checkErr
	}
	material, err := s.loadResultMaterialBounded(ctx, receipt.Request, limit)
	if err != nil {
		return verification.ResultSnapshot{}, 0, err
	}
	if current.Output != material.output {
		return verification.ResultSnapshot{}, 0, cortex.ErrRunConflict
	}
	snapshot := material.snapshot(receipt, s.clock.Now().UTC())
	if checkErr := snapshot.Validate(request, snapshot.ObservedAt); checkErr != nil {
		return verification.ResultSnapshot{}, 0, checkErr
	}
	return snapshot, material.bytes, nil
}
