package execution

import (
	"context"
	"encoding/json"

	"github.com/xraph/cortex/evidence"
)

// Recovery snapshots have explicit count and encoding bounds. Oversized journals
// cannot be represented as a truncated snapshot or treated as complete evidence.
const (
	MaxRunRecoveryEntries = 4096
	MaxRunRecoveryBytes   = 16 << 20
)

// RecoveredRunCall keeps a local observation and independent provider proof.
// Missing or contradictory proof is preserved for the adoption policy to reject.
type RecoveredRunCall struct {
	Call     RunCallRecord        `json:"call"`
	Provider *ProviderCallReceipt `json:"provider,omitempty"`
}

// RunRecoverySnapshot pins an exact sealed journal view. Its digest includes
// every call, local return, provider receipt, effect binding and effect receipt.
// Shape validation does not prove terminal outcomes, provider authenticity,
// verifier acceptance, current authority or permission to release capacity.
type RunRecoverySnapshot struct {
	SchemaVersion int                `json:"schema_version"`
	Seal          RunCallSeal        `json:"seal"`
	Calls         []RecoveredRunCall `json:"calls,omitempty"`
	Effects       []EffectRecord     `json:"effects,omitempty"`
	Digest        string             `json:"digest"`
}

// SealRunRecoverySnapshot validates membership, bounds and order, then detaches
// the entire observation. Legacy unbound effects and unresolved calls stay visible.
func SealRunRecoverySnapshot(s RunRecoverySnapshot) (RunRecoverySnapshot, error) {
	if s.SchemaVersion != 1 || s.Seal.Validate() != nil || len(s.Calls)+len(s.Effects) > MaxRunRecoveryEntries {
		return RunRecoverySnapshot{}, ErrInvalidRunCall
	}
	bindings := make(map[string]string, len(s.Calls))
	identities := make(map[string]bool, len(s.Calls))
	previous := ""
	used := 0
	for _, entry := range s.Calls {
		r := entry.Call.Request
		if entry.Call.Validate() != nil || r.RunID != s.Seal.Request.RunID || r.Scope.Canonical() != s.Seal.Request.Scope.Canonical() || r.Key() <= previous || identities[r.ID.String()] {
			return RunRecoverySnapshot{}, ErrInvalidRunCall
		}
		_, digest, err := r.Freeze()
		if err != nil {
			return RunRecoverySnapshot{}, err
		}
		if entry.Provider != nil {
			if _, _, err = entry.Provider.Freeze(r); err != nil {
				return RunRecoverySnapshot{}, err
			}
		}
		bindings[r.Key()] = digest
		identities[r.ID.String()] = true
		previous = r.Key()
		data, err := json.Marshal(entry)
		if err != nil {
			return RunRecoverySnapshot{}, ErrInvalidRunCall
		}
		used += len(data)
		if used > MaxRunRecoveryBytes {
			return RunRecoverySnapshot{}, ErrInvalidRunCall
		}
	}
	previous = ""
	for _, entry := range s.Effects {
		r, _, err := entry.Request.Freeze()
		key := evidence.Digest([]byte(r.OperationKey))
		if err != nil || r.RunID != s.Seal.Request.RunID || r.Scope.Canonical() != s.Seal.Request.Scope.Canonical() || key <= previous || (entry.Receipt != nil && entry.Receipt.Validate(r) != nil) {
			return RunRecoverySnapshot{}, ErrInvalidRunCall
		}
		if r.RunCall != nil {
			_, digest, freezeErr := r.RunCall.Freeze()
			if freezeErr != nil || bindings[r.RunCall.Key()] != digest {
				return RunRecoverySnapshot{}, ErrRunCallConflict
			}
		}
		previous = key
		data, encodeErr := json.Marshal(entry)
		if encodeErr != nil {
			return RunRecoverySnapshot{}, ErrInvalidRunCall
		}
		used += len(data)
		if used > MaxRunRecoveryBytes {
			return RunRecoverySnapshot{}, ErrInvalidRunCall
		}
	}
	s.Digest = ""
	data, err := json.Marshal(s)
	if err != nil || len(data) > MaxRunRecoveryBytes {
		return RunRecoverySnapshot{}, ErrInvalidRunCall
	}
	digest := evidence.Digest(data)
	if len(data)+len(digest) > MaxRunRecoveryBytes {
		return RunRecoverySnapshot{}, ErrInvalidRunCall
	}
	var frozen RunRecoverySnapshot
	if json.Unmarshal(data, &frozen) != nil {
		return RunRecoverySnapshot{}, ErrInvalidRunCall
	}
	frozen.Digest = digest
	return frozen, nil
}

// Validate checks integrity and complete binding, not readiness for adoption.
func (s RunRecoverySnapshot) Validate() error {
	frozen, err := SealRunRecoverySnapshot(s)
	if err != nil {
		return err
	}
	if frozen.Digest != s.Digest {
		return ErrInvalidRunCall
	}
	return nil
}

// RunRecoverySnapshotStore reads the entire sealed journal under one admission
// guard shared by local completions and terminal receipt writers. All writers
// must implement that protocol. Reads are bounded and exact-scope. The caller
// authorizes discovery; a snapshot alone never grants adoption or settlement.
type RunRecoverySnapshotStore interface {
	SnapshotRunRecovery(context.Context, RunCallSeal) (RunRecoverySnapshot, error)
}
