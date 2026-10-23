package event

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

// MaxTerminalSnapshotBytes bounds the encoded private terminal snapshot.
const MaxTerminalSnapshotBytes = 8 << 20

// TerminalUpdate binds one unchecked terminal snapshot to a stable event identity.
// Run contains private storage fields; only lifecycle metadata enters public replay.
type TerminalUpdate struct {
	SchemaVersion int     `json:"schema_version"`
	EventID       id.ID   `json:"event_id"`
	Run           run.Run `json:"run"`
}

// Freeze detaches the snapshot and binds retries to its complete contents.
// Required-verification success must use the guarded candidate/result transaction.
func (r TerminalUpdate) Freeze() (TerminalUpdate, string, error) {
	if r.SchemaVersion != 1 || r.EventID.IsNil() || r.EventID.Prefix() != PrefixEvent || !validRun(r.Run.ID) || r.Run.AgentID.IsNil() || r.Run.AgentID.Prefix() != id.PrefixAgent || cortex.ValidateScope(r.Run.Scope) != nil || r.Run.Revision < 0 || r.Run.Revision == math.MaxInt64 || r.Run.CompletedAt == nil || r.Run.CompletedAt.IsZero() || r.Run.VerificationStatus.Normalized() != verification.StatusUnchecked || r.Run.ResultRef != "" {
		return TerminalUpdate{}, "", ErrInvalid
	}
	switch r.Run.State {
	case run.StateCompleted:
		if r.Run.Provenance.VerificationRequired {
			return TerminalUpdate{}, "", ErrInvalid
		}
	case run.StateFailed, run.StateIncomplete, run.StateCancelled:
	default:
		return TerminalUpdate{}, "", ErrInvalid
	}
	if r.Run.Provenance.VerificationRequired && r.Run.Output != "" {
		return TerminalUpdate{}, "", ErrInvalid
	}
	body, err := json.Marshal(r)
	if err != nil {
		return TerminalUpdate{}, "", err
	}
	if len(body) > MaxTerminalSnapshotBytes {
		return TerminalUpdate{}, "", ErrInvalid
	}
	// Bypass Run.UnmarshalJSON here so metadata numbers retain exact precision.
	type runWire run.Run
	var frozen struct {
		SchemaVersion int     `json:"schema_version"`
		EventID       id.ID   `json:"event_id"`
		Run           runWire `json:"run"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if decodeErr := decoder.Decode(&frozen); decodeErr != nil {
		return TerminalUpdate{}, "", decodeErr
	}
	return TerminalUpdate{SchemaVersion: frozen.SchemaVersion, EventID: frozen.EventID, Run: run.Run(frozen.Run)}, evidence.Digest(body), nil
}

// TerminalReceipt acknowledges the atomic terminal state and metadata observation.
// It contains no output, error text or caller metadata.
type TerminalReceipt struct {
	SchemaVersion int           `json:"schema_version"`
	EventID       id.ID         `json:"event_id"`
	RunID         id.AgentRunID `json:"run_id"`
	RequestDigest string        `json:"request_digest"`
	RunRevision   int64         `json:"run_revision"`
	UpdatedAt     time.Time     `json:"updated_at"`
}

// TerminalStore is an optional exact-scope atomic run/event storage capability.
// Identical retries return the original receipt, including after event pruning.
// Implementations must roll back the run, receipt and event together on failure.
type TerminalStore interface {
	CommitTerminalRun(context.Context, TerminalUpdate) (TerminalReceipt, error)
}

// Validate checks a receipt against the complete original request.
func (r TerminalReceipt) Validate(request TerminalUpdate) error {
	_, digest, err := request.Freeze()
	if err != nil {
		return err
	}
	if r.SchemaVersion != 1 || r.EventID != request.EventID || r.RunID != request.Run.ID || r.RequestDigest != digest || r.RunRevision != request.Run.Revision+1 || r.UpdatedAt.IsZero() {
		return ErrInvalid
	}
	return nil
}
