package verification

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

const (
	PrefixResult      id.Prefix = "vresult"
	PrefixCommitEvent id.Prefix = "vcommit"
)

// ErrCommitUnavailable means the backend cannot provide an atomic result commit.
var ErrCommitUnavailable = cortex.ErrResultCommitUnavailable

// CommitRequest is trusted host input. You must fence current authorization and
// resource revisions throughout this operation. Storage checks saved bindings;
// it cannot discover changes in external services or authorize public delivery.
// Report order is significant for idempotency. Candidate text is never accepted here.
type CommitRequest struct {
	SchemaVersion       int       `json:"schema_version"`
	ResultID            id.ID     `json:"result_id"`
	ExpectedRunRevision int64     `json:"expected_run_revision"`
	ContractID          id.ID     `json:"contract_id"`
	ReportIDs           []id.ID   `json:"report_ids"`
	Current             Binding   `json:"current"`
	JobLease            *JobLease `json:"job_lease,omitempty"`
}

// Freeze validates, bounds and detaches the request before a transaction starts.
func (r CommitRequest) Freeze() (CommitRequest, string, error) {
	if r.SchemaVersion != 1 || r.ResultID.Prefix() != PrefixResult || r.ResultID.IsNil() || r.ContractID.Prefix() != PrefixContract || r.ContractID.IsNil() || r.ExpectedRunRevision <= 0 || r.ExpectedRunRevision == math.MaxInt64 || len(r.ReportIDs) == 0 || len(r.ReportIDs) > 256 {
		return CommitRequest{}, "", ErrInvalid
	}
	if r.JobLease != nil {
		if err := r.JobLease.Validate(); err != nil {
			return CommitRequest{}, "", err
		}
	}
	if err := r.Current.Validate(); err != nil {
		return CommitRequest{}, "", err
	}
	seen := make(map[id.ID]bool, len(r.ReportIDs))
	for _, reportID := range r.ReportIDs {
		if reportID.IsNil() || reportID.Prefix() != PrefixReport || seen[reportID] {
			return CommitRequest{}, "", ErrInvalid
		}
		seen[reportID] = true
	}
	digest, err := recordDigest(r)
	if err != nil {
		return CommitRequest{}, "", err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return CommitRequest{}, "", err
	}
	var frozen CommitRequest
	if decodeErr := json.Unmarshal(data, &frozen); decodeErr != nil {
		return CommitRequest{}, "", decodeErr
	}
	return frozen, digest, nil
}

// CommitOutbox is a durable metadata-only publication intent. Delivery still
// requires host authorization. EventID is stable across retries; this is not a
// public replay cursor and does not assign a public stream sequence.
type CommitOutbox struct {
	EventID       id.ID         `json:"event_id"`
	Type          string        `json:"type"`
	RunID         id.AgentRunID `json:"run_id"`
	ResultID      id.ID         `json:"result_id"`
	BindingDigest string        `json:"binding_digest"`
}

// CommitReceipt records a historical validation and its atomic outbox entry.
// ObservedAt is the validation time, not a guarantee of continuing freshness.
// Digest proves integrity only. Trust receipts loaded from your own store.
type CommitReceipt struct {
	SchemaVersion int           `json:"schema_version"`
	Request       CommitRequest `json:"request"`
	RequestDigest string        `json:"request_digest"`
	RunRevision   int64         `json:"run_revision"`
	ReportDigests []string      `json:"report_digests"`
	ObservedAt    time.Time     `json:"observed_at"`
	Outbox        CommitOutbox  `json:"outbox"`
	Digest        string        `json:"digest"`
}

// SealCommitReceipt validates and detaches a storage-produced receipt.
func SealCommitReceipt(r CommitReceipt) (CommitReceipt, error) {
	req, digest, err := r.Request.Freeze()
	if err != nil {
		return CommitReceipt{}, err
	}
	bindingDigest, err := req.Current.Digest()
	if err != nil {
		return CommitReceipt{}, err
	}
	if r.SchemaVersion != 1 || r.RequestDigest != digest || r.RunRevision != req.ExpectedRunRevision+1 || r.ObservedAt.IsZero() || len(r.ReportDigests) != len(req.ReportIDs) || r.Outbox.EventID.IsNil() || r.Outbox.EventID.Prefix() != PrefixCommitEvent || r.Outbox.Type != "result_committed" || r.Outbox.RunID != req.Current.RunID || r.Outbox.ResultID != req.ResultID || r.Outbox.BindingDigest != bindingDigest {
		return CommitReceipt{}, ErrInvalid
	}
	for _, reportDigest := range r.ReportDigests {
		if !evidence.ValidDigest(reportDigest) {
			return CommitReceipt{}, ErrInvalid
		}
	}
	r.Request = req
	r.ReportDigests = append([]string(nil), r.ReportDigests...)
	r.Digest = ""
	r.Digest, err = recordDigest(r)
	if err != nil {
		return CommitReceipt{}, err
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 1<<20 {
		return CommitReceipt{}, ErrInvalid
	}
	return r, nil
}

// Validate rejects a changed receipt without resealing it.
func (r CommitReceipt) Validate() error {
	sealed, err := SealCommitReceipt(r)
	if err != nil {
		return err
	}
	if sealed.Digest != r.Digest {
		return ErrInvalid
	}
	return nil
}

// CommitStore is optional and does not widen the legacy composite store.
// Same-request retries return the original historical receipt even after expiry.
// Callers must reauthorize every later read or delivery through their host policy.
type CommitStore interface {
	CommitVerifiedResult(context.Context, CommitRequest) (CommitReceipt, error)
	GetCommittedResult(context.Context, id.ID) (CommitReceipt, error)
}
