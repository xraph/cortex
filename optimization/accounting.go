package optimization

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
)

// OperationClass separates every source of metered experiment usage.
type OperationClass string

const (
	OperationBaseline    OperationClass = "baseline"
	OperationProposal    OperationClass = "proposal"
	OperationProbe       OperationClass = "probe"
	OperationComposition OperationClass = "composition"
	OperationValidation  OperationClass = "validation"
	OperationReflection  OperationClass = "reflection"
	OperationJudge       OperationClass = "judge"
	OperationEmbedding   OperationClass = "embedding"
	OperationRetrieval   OperationClass = "retrieval"
	OperationTool        OperationClass = "tool"
)

// ReservationState distinguishes conservative debits from settled receipts.
type ReservationState string

const (
	ReservationHeld    ReservationState = "held"
	ReservationUnknown ReservationState = "unknown"
	ReservationSettled ReservationState = "settled"
)

// UsageReservation binds an upper bound to one durable external call key.
type UsageReservation struct {
	ExperimentID    id.ID          `json:"experiment_id"`
	AttemptID       id.ID          `json:"attempt_id"`
	CallKey         string         `json:"call_key"`
	Operation       OperationClass `json:"operation"`
	MaxTokens       int64          `json:"max_tokens"`
	MaxUSDMicros    int64          `json:"max_usd_micros"`
	PricingRevision string         `json:"pricing_revision"`
	Lease           WorkLease      `json:"lease"`
	ExpiresAt       time.Time      `json:"expires_at"`
}

// Reservation records an upper-bound debit until authoritative settlement.
type Reservation struct {
	Call             *UsageCall        `json:"call,omitempty"`
	Request          UsageReservation  `json:"request"`
	State            ReservationState  `json:"state"`
	Revision         int64             `json:"revision"`
	Settlement       *UsageSettlement  `json:"settlement,omitempty"`
	History          []UsageSettlement `json:"history"`
	DebitedTokens    int64             `json:"debited_tokens"`
	DebitedUSDMicros int64             `json:"debited_usd_micros"`
	CreatedAt        time.Time         `json:"created_at"`
}

// UsageCall records that a provider operation may have started. It is written
// before dispatch and never grants a second dispatch after an unknown outcome.
type UsageCall struct {
	RequestDigest string    `json:"request_digest"`
	StartedAt     time.Time `json:"started_at"`
	Lease         WorkLease `json:"lease"`
}

// Validate checks the immutable request binding and durable dispatch authority.
func (call UsageCall) Validate() error {
	if !validDigest(call.RequestDigest) || call.StartedAt.IsZero() || call.Lease.Generation < 1 || !ValidOpaqueKey(call.Lease.Owner) {
		return ErrInvalidSpec
	}
	return nil
}

// UsageSettlement is idempotent by receipt and never hides observed overruns.
type UsageSettlement struct {
	ExperimentID id.ID     `json:"experiment_id"`
	CallKey      string    `json:"call_key"`
	AttemptID    id.ID     `json:"attempt_id"`
	Receipt      string    `json:"receipt"`
	Usage        Usage     `json:"usage"`
	Complete     bool      `json:"complete"`
	Lease        WorkLease `json:"lease"`
	ObservedAt   time.Time `json:"observed_at"`
}

// UsageReconciliation applies a verified late receipt without granting dispatch.
// ExpectedRevision fences concurrent observations; Actor is the recovery caller.
type UsageReconciliation struct {
	Settlement       UsageSettlement `json:"settlement"`
	RequestDigest    string          `json:"request_digest"`
	ExpectedRevision int64           `json:"expected_revision"`
	Actor            IdentityRef     `json:"actor"`
}

// Meter reserves before external dispatch and settles each receipt once.
type Meter interface {
	Reserve(context.Context, UsageReservation) (Reservation, error)
	Settle(context.Context, UsageSettlement) error
}

// UsageEstimate binds conservative token bounds to a resolved provider model.
type UsageEstimate struct {
	Model           string `json:"model"`
	ModelRevision   string `json:"model_revision"`
	PromptTokens    int64  `json:"prompt_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	PricingRevision string `json:"pricing_revision"`
}

// MoneyEstimate carries a conservative charge and its pricing provenance.
type MoneyEstimate struct {
	USDMicros       int64        `json:"usd_micros"`
	Quality         UsageQuality `json:"quality"`
	PricingRevision string       `json:"pricing_revision"`
}

// UsageEstimator operates on the actual provider request at the call boundary.
type UsageEstimator interface {
	Estimate(context.Context, *llm.Request) (UsageReservation, error)
}

// Pricing supplies host-approved bounds for the frozen pricing revision.
type Pricing interface {
	Price(context.Context, UsageEstimate) (MoneyEstimate, error)
}
