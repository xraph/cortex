package optimization

import (
	"context"
	"math/big"
)

// TokenPrice is a host-approved upper bound for one model and pricing revision.
// Rates are USD micro-units per million tokens. RequestUSDMicros covers a fixed
// per-call charge. Hosts must account for every billable category in these bounds
// or provide a different Pricing implementation. No rates are fetched implicitly.
type TokenPrice struct {
	Model                     string
	ModelRevision             string
	PricingRevision           string
	PromptUSDMicrosPerMillion int64
	OutputUSDMicrosPerMillion int64
	RequestUSDMicros          int64
}

// TokenPricing implements Pricing with immutable host-supplied token rates.
// A quote is an estimate, not evidence of the provider's final charge.
type TokenPricing struct{ rates TokenPrice }

var _ Pricing = (*TokenPricing)(nil)

// NewTokenPricing freezes the supplied rates. Explicit zero rates are supported
// for free or deterministic fixture providers; omitted revisions are refused.
func NewTokenPricing(rates TokenPrice) (*TokenPricing, error) {
	if !ValidOpaqueKey(rates.Model) || !ValidOpaqueKey(rates.ModelRevision) || !ValidOpaqueKey(rates.PricingRevision) || rates.PromptUSDMicrosPerMillion < 0 || rates.OutputUSDMicrosPerMillion < 0 || rates.RequestUSDMicros < 0 {
		return nil, ErrInvalidSpec
	}
	return &TokenPricing{rates: rates}, nil
}

// Price verifies the frozen model binding and rounds each token category's charge
// upward to a whole USD micro-unit before adding the fixed request charge.
// Intermediate arithmetic is exact; a result above int64 fails without a quote.
func (p *TokenPricing) Price(ctx context.Context, usage UsageEstimate) (MoneyEstimate, error) {
	if err := ctx.Err(); err != nil {
		return MoneyEstimate{}, err
	}
	if usage.Model != p.rates.Model || usage.ModelRevision != p.rates.ModelRevision || usage.PricingRevision != p.rates.PricingRevision || usage.PromptTokens < 0 || usage.OutputTokens < 0 {
		return MoneyEstimate{}, ErrInvalidSpec
	}
	prompt := roundedTokenCharge(usage.PromptTokens, p.rates.PromptUSDMicrosPerMillion)
	output := roundedTokenCharge(usage.OutputTokens, p.rates.OutputUSDMicrosPerMillion)
	var charge big.Int
	charge.Add(prompt, output)
	charge.Add(&charge, big.NewInt(p.rates.RequestUSDMicros))
	if !charge.IsInt64() {
		return MoneyEstimate{}, ErrInvalidSpec
	}
	if err := ctx.Err(); err != nil {
		return MoneyEstimate{}, err
	}
	return MoneyEstimate{USDMicros: charge.Int64(), Quality: UsageEstimated, PricingRevision: p.rates.PricingRevision}, nil
}

func roundedTokenCharge(tokens, rate int64) *big.Int {
	charge := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(rate))
	charge.Add(charge, big.NewInt(999_999))
	return charge.Quo(charge, big.NewInt(1_000_000))
}
