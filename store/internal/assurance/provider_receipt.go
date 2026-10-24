package assurance

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
)

const kindProviderCallReceipt = "provider_call_receipt"

var _ execution.ProviderCallReceiptStore = (*Repository)(nil)

// PutProviderCallReceipt retains authenticated proof without changing a local
// return or reservation. The caller owns authentication and the authority fence.
func (s *Repository) PutProviderCallReceipt(ctx context.Context, request execution.RunCallRequest, receipt execution.ProviderCallReceipt) error {
	if err := require(ctx, request.Scope); err != nil {
		return err
	}
	frozen, digest, err := receipt.Freeze(request)
	if err != nil {
		return err
	}
	if originalErr := s.requireOriginalRunCall(ctx, request); originalErr != nil {
		return originalErr
	}
	row, err := record(kindProviderCallReceipt, request.Key(), digest, request.RunID.String(), request.Scope, frozen)
	if err != nil {
		return err
	}
	if err = s.insert(ctx, row); errors.Is(err, ErrExists) {
		return execution.ErrRunCallConflict
	}
	return err
}

// GetProviderCallReceipt returns immutable proof, or nil for an unresolved call.
// It preserves contradictory local evidence for the recovery guard to reject.
func (s *Repository) GetProviderCallReceipt(ctx context.Context, request execution.RunCallRequest) (*execution.ProviderCallReceipt, error) {
	if err := require(ctx, request.Scope); err != nil {
		return nil, err
	}
	if err := s.requireOriginalRunCall(ctx, request); err != nil {
		return nil, err
	}
	row, err := s.get(ctx, Query{Kind: kindProviderCallReceipt, ID: request.Key()})
	if errors.Is(err, ErrAbsent) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var value execution.ProviderCallReceipt
	if row.Deleted || row.Kind != kindProviderCallReceipt || row.ID != request.Key() || row.RunID != request.RunID.String() || len(row.Body) > 16<<10 || row.Digest != evidence.Digest([]byte(row.Body)) || json.Unmarshal([]byte(row.Body), &value) != nil {
		return nil, execution.ErrInvalidRunCall
	}
	frozen, digest, err := value.Freeze(request)
	if err != nil || digest != row.Digest {
		return nil, execution.ErrInvalidRunCall
	}
	return &frozen, nil
}

func (s *Repository) requireOriginalRunCall(ctx context.Context, request execution.RunCallRequest) error {
	if request.Kind != execution.RunCallComplete && request.Kind != execution.RunCallStream {
		return execution.ErrInvalidRunCall
	}
	_, digest, err := request.Freeze()
	if err != nil {
		return err
	}
	existing, err := s.GetRunCall(ctx, request.RunID, request.OperationDigest)
	if err != nil {
		return err
	}
	_, original, err := existing.Request.Freeze()
	if err != nil || original != digest {
		return execution.ErrRunCallConflict
	}
	return nil
}
