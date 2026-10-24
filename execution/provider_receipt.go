package execution

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex/evidence"
)

// ProviderCallOutcome describes terminal provider knowledge. A transient lookup
// miss is unresolved and cannot be represented as a terminal receipt.
type ProviderCallOutcome string

const (
	ProviderCallSucceeded   ProviderCallOutcome = "succeeded"
	ProviderCallFailed      ProviderCallOutcome = "failed"
	ProviderCallNotAccepted ProviderCallOutcome = "not_accepted"
)

// ProviderCallReceipt binds host-authenticated proof to the full frozen journal
// request. References contain no credentials or result bytes. The host must
// authenticate provider/account/routing identity against original dispatch
// evidence, never current agent configuration. NotAccepted proves the original
// request cannot subsequently be accepted, including delayed delivery.
// ObservedAt is the host observation time, checked by its authenticator.
// Usage is authoritative only for marked token/money dimensions. A receipt
// does not settle reservations, authorize retry or accept a task output.
type ProviderCallReceipt struct {
	SchemaVersion   int                 `json:"schema_version"`
	RequestDigest   string              `json:"request_digest"`
	Outcome         ProviderCallOutcome `json:"outcome"`
	Provider        string              `json:"provider"`
	AdapterRevision string              `json:"adapter_revision"`
	AccountRef      string              `json:"account_ref"`
	OperationID     string              `json:"operation_id"`
	ProofRef        string              `json:"proof_ref"`
	ProofDigest     string              `json:"proof_digest"`
	ObservedAt      time.Time           `json:"observed_at"`
	ResultDigest    string              `json:"result_digest,omitempty"`
	Usage           ReservationUsage    `json:"usage"`
}

// Freeze checks shape and exact request binding, without authenticating proof.
// Successful result digests use the original invocation's result encoding.
func (c ProviderCallReceipt) Freeze(request RunCallRequest) (ProviderCallReceipt, string, error) {
	_, digest, err := request.Freeze()
	if err != nil || (request.Kind != RunCallComplete && request.Kind != RunCallStream) || c.SchemaVersion != 1 || c.RequestDigest != digest || !operationText(c.Provider, 256) || !operationText(c.AdapterRevision, 256) || !operationText(c.AccountRef, 1024) || !operationText(c.OperationID, 1024) || !operationText(c.ProofRef, 4096) || !evidence.ValidDigest(c.ProofDigest) || c.ObservedAt.IsZero() || c.Usage.Actual.Validate() != nil || c.Usage.Known & ^(KnownTokens|KnownMoney) != 0 {
		return ProviderCallReceipt{}, "", ErrInvalidRunCall
	}
	for i, value := range c.Usage.Actual.values() {
		if c.Usage.Known&(1<<i) == 0 && value != 0 {
			return ProviderCallReceipt{}, "", ErrInvalidRunCall
		}
	}
	switch c.Outcome {
	case ProviderCallSucceeded:
		if !evidence.ValidDigest(c.ResultDigest) {
			return ProviderCallReceipt{}, "", ErrInvalidRunCall
		}
	case ProviderCallFailed, ProviderCallNotAccepted:
		if c.ResultDigest != "" {
			return ProviderCallReceipt{}, "", ErrInvalidRunCall
		}
		if c.Outcome == ProviderCallNotAccepted && c.Usage.Actual != (Resources{}) {
			return ProviderCallReceipt{}, "", ErrInvalidRunCall
		}
	default:
		return ProviderCallReceipt{}, "", ErrInvalidRunCall
	}
	data, err := json.Marshal(c)
	if err != nil || len(data) > 16<<10 {
		return ProviderCallReceipt{}, "", ErrInvalidRunCall
	}
	var frozen ProviderCallReceipt
	if json.Unmarshal(data, &frozen) != nil {
		return ProviderCallReceipt{}, "", ErrInvalidRunCall
	}
	return frozen, evidence.Digest(data), nil
}

// ValidateObservation rejects contradictions with the current local journal.
// Call this again before adoption: a local return can arrive after reconciliation.
// Reported token counts remain observations and need not equal billable usage.
func (c ProviderCallReceipt) ValidateObservation(record RunCallRecord) error {
	if _, _, err := c.Freeze(record.Request); err != nil {
		return err
	}
	if record.Validate() != nil || c.ObservedAt.Before(record.StartedAt) {
		return ErrInvalidRunCall
	}
	if record.Completion != nil && (c.Outcome != ProviderCallSucceeded || c.ResultDigest != record.Completion.ResultDigest) {
		return ErrRunCallConflict
	}
	return nil
}

// ProviderCallReceiptStore retains immutable terminal proof separately from
// local returns. Get returns nil when the original call exists without proof.
// Writes require a host authority fence and authenticated proof. Storage checks
// shape and original request identity, not provider authenticity. Consumers must
// compare the latest local observation before adoption. Both records survive a
// disagreement; neither record permits dispatch or releases shared capacity.
type ProviderCallReceiptStore interface {
	RunCallStore
	PutProviderCallReceipt(context.Context, RunCallRequest, ProviderCallReceipt) error
	GetProviderCallReceipt(context.Context, RunCallRequest) (*ProviderCallReceipt, error)
}
