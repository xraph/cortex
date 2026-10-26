package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

const (
	// EffectActionRetry and EffectActionCompensate require distinct current grants.
	EffectActionRetry      EffectAction = "retry"
	EffectActionCompensate EffectAction = "compensate"
	// MaxEffectContinuations bounds seven additional attempts and eight compensations.
	MaxEffectContinuations = 15
	ContinuationAbsence    = "permanent_absence"
	ContinuationIdempotent = "provider_idempotency"
	ContinuationApplied    = "applied_compensation"
	compensationKeyPrefix  = "cortex/compensation/"
)

// EffectContinuationProof is a host-authenticated provider attestation. The host
// must verify the exact predecessor, permanently exclude the original worker,
// authenticate the stated absence or idempotency guarantee, and enforce the
// reserved spend ceiling. A digest or model assertion alone is not proof.
type EffectContinuationProof struct {
	Mode       string    `json:"mode"`
	Issuer     string    `json:"issuer"`
	Revision   string    `json:"revision"`
	Reference  string    `json:"reference"`
	Digest     string    `json:"digest"`
	ObservedAt time.Time `json:"observed_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// EffectContinuationRequest describes an independently charged external action.
// Original stays immutable. Operation has no RunCall and cannot reopen a sealed
// journal; its RunID retains provenance only. Compensation never proves rollback
// or success of the original task. Sequence and PredecessorDigest fence retries.
type EffectContinuationRequest struct {
	SchemaVersion     int                     `json:"schema_version"`
	ResultID          id.ID                   `json:"result_id"`
	ResultRetention   time.Duration           `json:"result_retention"`
	Original          EffectRequest           `json:"original"`
	Operation         EffectRequest           `json:"operation"`
	Sequence          int                     `json:"sequence"`
	Action            EffectAction            `json:"action"`
	ClaimDigest       string                  `json:"claim_digest"`
	Seal              RunCallSeal             `json:"seal"`
	SnapshotDigest    string                  `json:"snapshot_digest"`
	PredecessorDigest string                  `json:"predecessor_digest"`
	Proof             EffectContinuationProof `json:"proof"`
	Reservation       ReservationRequest      `json:"reservation"`
}

// CompensationOperationKey is reserved for explicit continuation admissions.
func CompensationOperationKey(original EffectRequest) string {
	return compensationKeyPrefix + evidence.Digest([]byte(original.OperationKey))
}

// IsCompensationOperationKey identifies a key unavailable to ordinary dispatch.
func IsCompensationOperationKey(key string) bool {
	return strings.HasPrefix(key, compensationKeyPrefix)
}

// Key identifies one immutable sequence position in the original operation.
func (r EffectContinuationRequest) Key() string {
	return evidence.Digest([]byte(r.Original.OperationKey)) + fmt.Sprintf(":%02d", r.Sequence)
}

// Freeze validates and detaches a request without authenticating its proof.
func (r EffectContinuationRequest) Freeze() (EffectContinuationRequest, string, error) {
	o, _, err := r.Original.Freeze()
	if err != nil || o.RunCall == nil || IsCompensationOperationKey(o.OperationKey) {
		return EffectContinuationRequest{}, "", ErrInvalidEffect
	}
	op, _, err := r.Operation.Freeze()
	if err != nil || r.SchemaVersion != 1 || r.ResultID.IsNil() || r.ResultID.Prefix() != evidence.PrefixArtifact || r.ResultRetention < time.Hour || r.ResultRetention > 365*24*time.Hour || r.Sequence < 1 || r.Sequence > MaxEffectContinuations || op.RunCall != nil || op.AttemptID == o.AttemptID || op.RunID != o.RunID || op.Scope.Canonical() != o.Scope.Canonical() || !evidence.ValidDigest(r.ClaimDigest) || r.Seal.Validate() != nil || r.Seal.Request.RunID != o.RunID || r.Seal.Request.Scope.Canonical() != o.Scope.Canonical() || !evidence.ValidDigest(r.SnapshotDigest) || !evidence.ValidDigest(r.PredecessorDigest) {
		return EffectContinuationRequest{}, "", ErrInvalidEffect
	}
	p := r.Proof
	if (p.Mode != ContinuationAbsence && p.Mode != ContinuationIdempotent && p.Mode != ContinuationApplied) || !operationText(p.Issuer, 256) || !operationText(p.Revision, 256) || !operationText(p.Reference, 4096) || !evidence.ValidDigest(p.Digest) || p.ObservedAt.Before(r.Seal.At) || !p.ExpiresAt.After(p.ObservedAt) || p.ExpiresAt.Sub(p.ObservedAt) > 30*time.Minute || op.Deadline.After(p.ExpiresAt) {
		return EffectContinuationRequest{}, "", ErrInvalidEffect
	}
	switch r.Action {
	case EffectActionRetry:
		if op.OperationKey != o.OperationKey || op.ArgsDigest != o.ArgsDigest || op.Requirement != o.Requirement || p.Mode == ContinuationApplied || o.Requirement.Retry.MaxAttempts < 2 {
			return EffectContinuationRequest{}, "", ErrInvalidEffect
		}
	case EffectActionCompensate:
		if o.Requirement.Class != EffectCompensatable || op.OperationKey != CompensationOperationKey(o) || op.Requirement.Tool != o.Requirement.Compensation.Tool || op.Requirement.Revision != o.Requirement.Compensation.Revision {
			return EffectContinuationRequest{}, "", ErrInvalidEffect
		}
	default:
		return EffectContinuationRequest{}, "", ErrInvalidEffect
	}
	if p.Mode == ContinuationIdempotent && (op.Requirement.Class != EffectIdempotent || p.Revision != op.Requirement.IdempotencyRevision) {
		return EffectContinuationRequest{}, "", ErrInvalidEffect
	}
	b, _, err := r.Reservation.Freeze()
	if err != nil || b.Scope.Canonical() != o.Scope.Canonical() || b.PlanID.Prefix() != "plan" || b.TaskID.Prefix() != "ptask" || b.OwnerRef != op.AttemptID.String() || !b.ParentID.IsNil() || b.Kind != ReservationLeaf || b.Allocation.ToolCalls != 1 || b.Allocation.Steps != 0 || b.Allocation.Tokens != 0 || b.Allocation.Replans != 0 || op.Deadline.After(b.Deadline) {
		return EffectContinuationRequest{}, "", ErrInvalidEffect
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 64<<10 {
		return EffectContinuationRequest{}, "", ErrInvalidEffect
	}
	var detached EffectContinuationRequest
	if json.Unmarshal(data, &detached) != nil {
		return EffectContinuationRequest{}, "", ErrInvalidEffect
	}
	return detached, evidence.Digest(data), nil
}

// EffectContinuation retains intent before dispatch and its optional terminal
// observation. Fresh is process-local permission and is never serialized.
type EffectContinuation struct {
	Request       EffectContinuationRequest `json:"request"`
	RequestDigest string                    `json:"request_digest"`
	Reservation   Reservation               `json:"reservation"`
	At            time.Time                 `json:"at"`
	Fresh         bool                      `json:"-"`
}

// Validate checks immutable intent and its independent reservation.
func (a EffectContinuation) Validate() error {
	_, d, err := a.Request.Freeze()
	_, rd, re := a.Request.Reservation.Freeze()
	if err != nil || re != nil || d != a.RequestDigest || a.Reservation.Validate() != nil || a.Reservation.RequestDigest != rd || a.At.IsZero() || !a.At.Equal(a.Reservation.CreatedAt) || a.Reservation.Request.ID != a.Request.Reservation.ID {
		return ErrInvalidEffect
	}
	return nil
}

// EffectContinuationResult is an observation requiring normal task verification.
// It is never an acceptance receipt. Unknown billing stays fully charged.
type EffectContinuationResult struct {
	RequestDigest string            `json:"request_digest"`
	Evidence      evidence.Ref      `json:"evidence"`
	Receipt       EffectReceipt     `json:"receipt"`
	Settlement    SettlementReceipt `json:"settlement"`
	At            time.Time         `json:"at"`
}

// Validate checks receipt and accounting bindings against the retained admission.
func (r EffectContinuationResult) Validate(a EffectContinuation) error {
	_, settlementDigest, settlementErr := r.Settlement.Request.Freeze()
	if settlementErr != nil || settlementDigest != r.Settlement.Digest || a.Validate() != nil || r.RequestDigest != a.RequestDigest || r.Evidence.Validate() != nil || r.Evidence.ID != a.Request.ResultID || r.Evidence.Scope.Canonical() != a.Request.Original.Scope.Canonical() || r.Evidence.Producer != "cortex.effect-continuation/v1" || !r.Evidence.CreatedAt.Equal(r.At) || r.Receipt.Validate(a.Request.Operation) != nil || r.Settlement.Request.ReservationID != a.Reservation.Request.ID || r.Settlement.Request.RequestDigest != a.Reservation.RequestDigest || r.At.Before(a.At) || !r.At.Equal(r.Settlement.At) {
		return ErrInvalidEffect
	}
	return nil
}

// EffectPredecessorDigest binds provider proof to the exact preceding observation.
func EffectPredecessorDigest(request EffectRequest, receipt *EffectReceipt) (string, error) {
	if _, _, err := request.Freeze(); err != nil {
		return "", err
	}
	if receipt != nil && receipt.Validate(request) != nil {
		return "", ErrInvalidEffect
	}
	data, err := json.Marshal(EffectRecord{Request: request, Receipt: receipt})
	if err != nil {
		return "", err
	}
	return evidence.Digest(data), nil
}

// EffectContinuationStore atomically reserves shared capacity and exact sequence
// before dispatch, then stores terminal observation and conservative settlement.
// Historical admission retries never return Fresh, including lost acknowledgments.
type EffectContinuationStore interface {
	AdmitEffectContinuation(context.Context, EffectContinuationRequest) (EffectContinuation, error)
	ListEffectContinuations(context.Context, EffectRequest) ([]EffectContinuation, error)
	GetEffectContinuationResult(context.Context, EffectContinuationRequest) (*EffectContinuationResult, error)
	CompleteEffectContinuation(context.Context, EffectContinuationRequest, EffectReceipt) (EffectContinuationResult, error)
}
