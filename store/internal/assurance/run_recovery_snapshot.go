package assurance

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
)

var _ execution.RunRecoverySnapshotStore = (*Repository)(nil)

// SnapshotRunRecovery reads all retained evidence under the run journal guard.
// All writers must share this protocol. A failed or ambiguous transaction returns
// no snapshot; callers must authenticate proof and authorize any later adoption.
func (s *Repository) SnapshotRunRecovery(ctx context.Context, seal execution.RunCallSeal) (execution.RunRecoverySnapshot, error) {
	if err := require(ctx, seal.Request.Scope); err != nil {
		return execution.RunRecoverySnapshot{}, err
	}
	if err := seal.Validate(); err != nil {
		return execution.RunRecoverySnapshot{}, err
	}
	frozen, _, err := seal.Request.Freeze()
	if err != nil {
		return execution.RunRecoverySnapshot{}, err
	}
	seal.Request = frozen
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result execution.RunRecoverySnapshot
	err = s.runCallTransaction(bounded, seal.Request.RunID, func(tc context.Context, tx *Repository) error {
		result = execution.RunRecoverySnapshot{}
		retained, readErr := tx.GetRunCallSeal(tc, seal.Request.RunID)
		if readErr != nil {
			return readErr
		}
		if retained == nil || retained.RequestDigest != seal.RequestDigest || !retained.At.Equal(seal.At) {
			return execution.ErrRunCallConflict
		}
		candidate := execution.RunRecoverySnapshot{SchemaVersion: 1, Seal: *retained}
		if scanErr := tx.readRecoveryEvidence(tc, &candidate); scanErr != nil {
			return scanErr
		}
		result, readErr = execution.SealRunRecoverySnapshot(candidate)
		return readErr
	})
	if err != nil {
		return execution.RunRecoverySnapshot{}, err
	}
	return result, nil
}

func (s *Repository) readRecoveryEvidence(ctx context.Context, result *execution.RunRecoverySnapshot) error {
	const pageSize = 128
	used := 0
	// Bound accumulation, not just the final encoded object. The final seal also
	// checks JSON wrapper overhead and validates cross-journal membership.
	account := func(value any) error {
		if len(result.Calls)+len(result.Effects) >= execution.MaxRunRecoveryEntries {
			return execution.ErrInvalidRunCall
		}
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		used += len(data)
		if used > execution.MaxRunRecoveryBytes {
			return execution.ErrInvalidRunCall
		}
		return nil
	}
	after := ""
	for {
		calls, err := s.ListRunCalls(ctx, execution.RunCallListRequest{RunID: result.Seal.Request.RunID, After: after, Limit: pageSize})
		if err != nil {
			return err
		}
		for _, call := range calls {
			entry := execution.RecoveredRunCall{Call: call}
			if call.Request.Kind != execution.RunCallTool {
				entry.Provider, err = s.GetProviderCallReceipt(ctx, call.Request)
				if err != nil {
					return err
				}
			}
			if sizeErr := account(entry); sizeErr != nil {
				return sizeErr
			}
			result.Calls = append(result.Calls, entry)
			after = call.Request.Key()
		}
		if len(calls) < pageSize {
			break
		}
	}
	after = ""
	for {
		effects, err := s.ListEffects(ctx, execution.EffectListRequest{RunID: result.Seal.Request.RunID, After: after, Limit: pageSize})
		if err != nil {
			return err
		}
		for _, effect := range effects {
			if sizeErr := account(effect); sizeErr != nil {
				return sizeErr
			}
			result.Effects = append(result.Effects, effect)
			after = evidence.Digest([]byte(effect.Request.OperationKey))
		}
		if len(effects) < pageSize {
			break
		}
	}
	return ctx.Err()
}
