package optimization

import (
	"context"
	"math"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/llm"
)

// InputTokenBound includes all provider input framing, messages and tool schemas.
// Bounded is a host assertion about an upper bound, not an approximate count.
type InputTokenBound struct {
	Model         string
	ModelRevision string
	Tokens        int64
	Bounded       bool
}

// InputTokenCounter supplies a local, model-specific conservative input bound.
// Implementations must include provider-added framing and cannot make unmetered
// charged calls. Unknown tokenizer or model revisions must return Bounded=false.
type InputTokenCounter interface {
	Bound(context.Context, *llm.Request) (InputTokenBound, error)
}

// RequestEstimatorConfig binds estimates to one durable logical call. The caller
// assigns its stable call key before dispatch; retries must reuse that identity.
type RequestEstimatorConfig struct {
	Scope           cortex.Scope
	Reservation     UsageReservation
	Model           string
	ModelRevision   string
	TokenOnly       bool
	MaxRequestBytes int
}

// RequestEstimator computes the full token and monetary reservation at dispatch.
type RequestEstimator struct {
	counter InputTokenCounter
	pricing Pricing
	config  RequestEstimatorConfig
}

// NewRequestEstimator requires explicit token-only mode or frozen pricing.
func NewRequestEstimator(counter InputTokenCounter, pricing Pricing, config RequestEstimatorConfig) (*RequestEstimator, error) {
	if counter == nil || !ValidOpaqueKey(config.Model) || !ValidOpaqueKey(config.ModelRevision) || config.MaxRequestBytes <= 0 || !ValidOpaqueKey(config.Reservation.CallKey) {
		return nil, ErrInvalidSpec
	}
	if _, err := ScopeKey(config.Scope); err != nil {
		return nil, err
	}
	if _, err := ParseID(config.Reservation.ExperimentID.String(), PrefixExperiment); err != nil {
		return nil, err
	}
	if _, err := ParseID(config.Reservation.AttemptID.String(), PrefixAttempt); err != nil {
		return nil, err
	}
	if config.Reservation.ExpiresAt.IsZero() || config.Reservation.Lease.ExperimentID != config.Reservation.ExperimentID || config.Reservation.Lease.Generation < 1 || !ValidOpaqueKey(config.Reservation.Lease.Owner) {
		return nil, ErrInvalidSpec
	}
	if config.TokenOnly {
		if pricing != nil || config.Reservation.PricingRevision != "" {
			return nil, ErrInvalidSpec
		}
	} else if pricing == nil || !ValidOpaqueKey(config.Reservation.PricingRevision) {
		return nil, ErrInvalidSpec
	}
	config.Scope = copyScope(config.Scope)
	return &RequestEstimator{counter: counter, pricing: pricing, config: config}, nil
}

// Estimate includes input and the provider output cap, refusing unknown bounds.
func (e *RequestEstimator) Estimate(ctx context.Context, req *llm.Request) (UsageReservation, error) {
	var empty UsageReservation
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if cortex.ScopeFromContext(ctx).IsZero() {
		return empty, cortex.ErrNoScope
	}
	key, err := ScopeKey(cortex.ScopeFromContext(ctx))
	if err != nil {
		return empty, err
	}
	if key != e.config.Scope.Canonical() {
		return empty, ErrPolicyDenied
	}
	if req == nil || req.Model != e.config.Model || req.MaxTokens <= 0 {
		return empty, ErrInvalidSpec
	}
	output := int64(req.MaxTokens)
	snapshot, err := copyModelJSON(req, e.config.MaxRequestBytes)
	if err != nil {
		return empty, err
	}
	bound, err := e.counter.Bound(copyScopeContext(ctx), snapshot)
	if err != nil {
		return empty, err
	}
	if !bound.Bounded || bound.Model != e.config.Model || bound.ModelRevision != e.config.ModelRevision || bound.Tokens < 0 || bound.Tokens > math.MaxInt64-output {
		return empty, ErrIncompleteEvidence
	}
	result := e.config.Reservation
	result.MaxTokens, result.MaxUSDMicros = bound.Tokens+output, 0
	if !e.config.TokenOnly {
		price, priceErr := e.pricing.Price(copyScopeContext(ctx), UsageEstimate{Model: e.config.Model, ModelRevision: e.config.ModelRevision, PromptTokens: bound.Tokens, OutputTokens: output, PricingRevision: result.PricingRevision})
		if priceErr != nil {
			return empty, priceErr
		}
		if price.USDMicros < 0 || price.PricingRevision != result.PricingRevision || (price.Quality != UsageKnown && price.Quality != UsageEstimated) {
			return empty, ErrIncompleteEvidence
		}
		result.MaxUSDMicros = price.USDMicros
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	return result, nil
}
