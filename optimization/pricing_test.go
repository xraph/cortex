package optimization_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/xraph/cortex/optimization"
)

func tokenPrice() optimization.TokenPrice {
	return optimization.TokenPrice{Model: "fixture", ModelRevision: "model-1", PricingRevision: "price-1", PromptUSDMicrosPerMillion: 2_000_000, OutputUSDMicrosPerMillion: 5_000_000, RequestUSDMicros: 7}
}

func TestTokenPricingUsesFrozenRatesAndRoundsUp(t *testing.T) {
	for _, tc := range []struct {
		name                                               string
		prompt, output, inputRate, outputRate, fixed, want int64
	}{
		{"normal", 100, 10, 2_000_000, 5_000_000, 7, 257},
		{"fraction", 1, 1, 1, 1, 0, 2},
		{"whole", 1_000_000, 1_000_000, 1, 2, 3, 6},
		{"zero", 100, 50, 0, 0, 0, 0},
		{"large-intermediate", math.MaxInt64, 0, 1_000_000, 0, 0, math.MaxInt64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := tokenPrice()
			config.PromptUSDMicrosPerMillion, config.OutputUSDMicrosPerMillion, config.RequestUSDMicros = tc.inputRate, tc.outputRate, tc.fixed
			pricing, err := optimization.NewTokenPricing(config)
			if err != nil {
				t.Fatal(err)
			}
			config.PromptUSDMicrosPerMillion = 999 // Configuration is frozen at construction.
			quote, err := pricing.Price(context.Background(), optimization.UsageEstimate{Model: "fixture", ModelRevision: "model-1", PricingRevision: "price-1", PromptTokens: tc.prompt, OutputTokens: tc.output})
			if err != nil || quote.USDMicros != tc.want || quote.Quality != optimization.UsageEstimated || quote.PricingRevision != "price-1" {
				t.Fatalf("quote: %+v %v", quote, err)
			}
		})
	}
}

func TestTokenPricingRejectsUnboundAndOverflowingQuotes(t *testing.T) {
	for _, mode := range []string{"model", "model-revision", "price-revision", "negative-input", "negative-output", "overflow", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			pricing, err := optimization.NewTokenPricing(tokenPrice())
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			estimate := optimization.UsageEstimate{Model: "fixture", ModelRevision: "model-1", PricingRevision: "price-1", PromptTokens: 10, OutputTokens: 10}
			switch mode {
			case "model":
				estimate.Model = "other"
			case "model-revision":
				estimate.ModelRevision = "other"
			case "price-revision":
				estimate.PricingRevision = "other"
			case "negative-input":
				estimate.PromptTokens = -1
			case "negative-output":
				estimate.OutputTokens = -1
			case "overflow":
				estimate.PromptTokens = math.MaxInt64
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			quote, err := pricing.Price(ctx, estimate)
			if err == nil || quote != (optimization.MoneyEstimate{}) {
				t.Fatalf("invalid quote accepted: %+v %v", quote, err)
			}
			if mode == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
		})
	}
}

func TestTokenPricingRejectsInvalidRates(t *testing.T) {
	for _, mode := range []string{"model", "model-revision", "price-revision", "input-rate", "output-rate", "request-rate"} {
		t.Run(mode, func(t *testing.T) {
			config := tokenPrice()
			switch mode {
			case "model":
				config.Model = ""
			case "model-revision":
				config.ModelRevision = ""
			case "price-revision":
				config.PricingRevision = ""
			case "input-rate":
				config.PromptUSDMicrosPerMillion = -1
			case "output-rate":
				config.OutputUSDMicrosPerMillion = -1
			case "request-rate":
				config.RequestUSDMicros = -1
			}
			if _, err := optimization.NewTokenPricing(config); !errors.Is(err, optimization.ErrInvalidSpec) {
				t.Fatalf("invalid rates accepted: %v", err)
			}
		})
	}
}
