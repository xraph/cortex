package verification

import (
	"context"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

// ResultReadRequest pins a historical receipt to its exact run. A valid digest
// is an integrity binding, never an access credential.
type ResultReadRequest struct {
	SchemaVersion int           `json:"schema_version"`
	RunID         id.AgentRunID `json:"run_id"`
	ResultID      id.ID         `json:"result_id"`
	ReceiptDigest string        `json:"receipt_digest"`
}

// Validate rejects unbound reads before storage is consulted.
func (r ResultReadRequest) Validate() error {
	if r.SchemaVersion != 1 || r.RunID.IsNil() || r.RunID.Prefix() != id.PrefixAgentRun || r.ResultID.IsNil() || r.ResultID.Prefix() != PrefixResult || !evidence.ValidDigest(r.ReceiptDigest) {
		return ErrInvalid
	}
	return nil
}

// ResultSnapshot contains private, stored result material from one transaction.
// Output is intentionally excluded from JSON. This is not a public response:
// hosts must check current grants and source revisions and apply publication
// policy before exposing content. Reports allow time-based revalidation after I/O.
type ResultSnapshot struct {
	SchemaVersion int           `json:"schema_version"`
	Receipt       CommitReceipt `json:"receipt"`
	Contract      Contract      `json:"contract"`
	Reports       []Report      `json:"reports"`
	ObservedAt    time.Time     `json:"observed_at"`
	Output        string        `json:"-"`
}

// Validate checks identity, bytes and freshness at the supplied trusted time.
// It cannot establish current authority or discover external source changes.
func (s ResultSnapshot) Validate(request ResultReadRequest, at time.Time) error {
	if request.Validate() != nil || s.SchemaVersion != 1 || s.Receipt.Validate() != nil || s.Contract.Validate() != nil || s.ObservedAt.IsZero() || at.Before(s.ObservedAt) || s.ObservedAt.Before(s.Receipt.ObservedAt) {
		return ErrInvalid
	}
	receipt := s.Receipt
	binding := receipt.Request.Current
	if receipt.Digest != request.ReceiptDigest || receipt.Request.ResultID != request.ResultID || binding.RunID != request.RunID || s.Contract.ID != receipt.Request.ContractID || s.Contract.Digest != binding.ContractDigest || len(s.Reports) != len(receipt.ReportDigests) || len(s.Reports) != len(receipt.Request.ReportIDs) {
		return ErrConflict
	}
	if len(s.Output) > 1<<20 || int64(len(s.Output)) != binding.Candidate.Bytes || evidence.Digest([]byte(s.Output)) != binding.Candidate.SHA256 {
		return ErrConflict
	}
	for i, report := range s.Reports {
		if report.ID != receipt.Request.ReportIDs[i] || report.Digest != receipt.ReportDigests[i] || CheckFreshness(report, binding, at) != StatusPassed {
			return ErrConflict
		}
	}
	if Aggregate(s.Contract, s.Reports, at).Status != StatusPassed {
		return ErrConflict
	}
	return nil
}

// ResultSnapshotStore is optional trusted storage, not a public content reader.
// It rechecks the current passed run, immutable receipt, saved evidence and report
// freshness under one run transaction. No remote callbacks run under database
// locks. External authorization and publication remain the host's responsibility.
type ResultSnapshotStore interface {
	ReadResultSnapshot(context.Context, ResultReadRequest) (ResultSnapshot, error)
}
