package execution

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

// PrefixEffectAttempt identifies the original dispatch, retained across recovery.
const PrefixEffectAttempt id.Prefix = "effat"

var (
	ErrEffectConflict    = cortex.ErrEffectConflict
	ErrEffectNotFound    = cortex.ErrEffectNotFound
	ErrEffectUncertain   = cortex.ErrEffectUncertain
	ErrEffectUnavailable = cortex.ErrEffectUnavailable
)

// EffectRequest is a host-bound logical operation. OperationKey is unique across
// tools in its exact scope. Retain the entire request, including AttemptID, for
// retries and worker replacement. Arguments are hashed, not stored in the journal.
// AuthorityRef identifies current authorization; it is never a bearer credential.
type EffectRequest struct {
	SchemaVersion int               `json:"schema_version"`
	Scope         cortex.Scope      `json:"scope"`
	RunID         id.AgentRunID     `json:"run_id"`
	OperationKey  string            `json:"operation_key"`
	AttemptID     id.ID             `json:"attempt_id"`
	ArgsDigest    string            `json:"args_digest"`
	PrincipalRef  string            `json:"principal_ref"`
	AuthorityRef  string            `json:"authority_ref"`
	Deadline      time.Time         `json:"deadline"`
	Requirement   EffectRequirement `json:"requirement"`
}

// Freeze validates and detaches the request and hashes its canonical encoding.
// Deadline expiry is checked at dispatch, so expired operations remain readable
// and can still be reconciled with fresh host authorization.
func (r EffectRequest) Freeze() (EffectRequest, string, error) {
	if r.SchemaVersion != 1 || cortex.ValidateScope(r.Scope) != nil || r.RunID.IsNil() || r.RunID.Prefix() != id.PrefixAgentRun || r.AttemptID.IsNil() || r.AttemptID.Prefix() != PrefixEffectAttempt || !evidence.ValidDigest(r.ArgsDigest) || !operationText(r.OperationKey, 256) || !operationText(r.PrincipalRef, 512) || !operationText(r.AuthorityRef, 512) || r.Deadline.IsZero() || r.Requirement.Validate() != nil {
		return EffectRequest{}, "", ErrInvalidEffect
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 16<<10 {
		return EffectRequest{}, "", ErrInvalidEffect
	}
	var frozen EffectRequest
	if err = json.Unmarshal(data, &frozen); err != nil {
		return EffectRequest{}, "", ErrInvalidEffect
	}
	return frozen, evidence.Digest(data), nil
}
func operationText(value string, limit int) bool {
	return len(value) <= limit && strings.TrimSpace(value) == value && effectText(value)
}

// EffectOutcome distinguishes terminal provider knowledge from ambiguity.
type EffectOutcome string

const (
	EffectApplied    EffectOutcome = "applied"
	EffectNotApplied EffectOutcome = "not_applied"
	EffectUnknown    EffectOutcome = "unknown"
)

// EffectReceipt is trusted adapter evidence about the exact request. NotApplied
// must prove the original dispatch cannot later apply, not merely that a read
// currently finds no object. EvidenceRef identifies retained provider evidence.
// Applied does not establish task acceptance or correctness of the artifact.
type EffectReceipt struct {
	SchemaVersion   int           `json:"schema_version"`
	RequestDigest   string        `json:"request_digest"`
	Outcome         EffectOutcome `json:"outcome"`
	ExternalID      string        `json:"external_id,omitempty"`
	ExternalVersion string        `json:"external_version,omitempty"`
	EvidenceRef     string        `json:"evidence_ref"`
}

// Validate binds terminal knowledge to the complete operation request.
func (v EffectReceipt) Validate(r EffectRequest) error {
	_, digest, err := r.Freeze()
	if err != nil || v.SchemaVersion != 1 || v.RequestDigest != digest || !operationText(v.EvidenceRef, 4096) {
		return ErrInvalidEffect
	}
	if v.Outcome != EffectApplied && v.Outcome != EffectNotApplied {
		return ErrInvalidEffect
	}
	if v.Outcome == EffectApplied && !operationText(v.ExternalID, 1024) {
		return ErrInvalidEffect
	}
	if v.ExternalID != "" && !operationText(v.ExternalID, 1024) || v.ExternalVersion != "" && !operationText(v.ExternalVersion, 1024) {
		return ErrInvalidEffect
	}
	return nil
}

// EffectStatus is a read-only authoritative probe. Unknown carries no receipt.
type EffectStatus struct {
	Outcome EffectOutcome
	Receipt *EffectReceipt
}

// EffectRecord retains the original operation and optional terminal receipt.
type EffectRecord struct {
	Request EffectRequest  `json:"request"`
	Receipt *EffectReceipt `json:"receipt,omitempty"`
}

// EffectStore is optional durable effect persistence. BeginEffect returns true
// only to the successful first insert, never on replay or an uncertain database
// acknowledgement. Same-key changes conflict. CompleteEffect is immutable and
// idempotent only for the identical receipt. Every method uses exact context
// scope. Rows must outlive all possible delivery retries and provider effects.
// These methods are host infrastructure, not directly exposed to model tools.
type EffectStore interface {
	BeginEffect(context.Context, EffectRequest) (bool, error)
	GetEffect(context.Context, string) (EffectRecord, error)
	CompleteEffect(context.Context, EffectRequest, EffectReceipt) error
}

// EffectAdapter is a trusted registered handler. Execute must enforce current
// authorization at its external write boundary, honor deadlines, and use the
// stable OperationKey for provider idempotency when declared. Reconcile is
// read-only and must never dispatch, compensate or infer failure from absence.
// It must bind provider evidence to the full request and original attempt.
type EffectAdapter interface {
	Execute(context.Context, EffectRequest, []byte) (EffectReceipt, error)
	Reconcile(context.Context, EffectRequest) (EffectStatus, error)
}
