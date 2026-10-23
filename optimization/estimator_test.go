package optimization_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/optimization"
)

type inputBound func(context.Context, *llm.Request) (optimization.InputTokenBound, error)

func (f inputBound) Bound(ctx context.Context, req *llm.Request) (optimization.InputTokenBound, error) {
	return f(ctx, req)
}

func TestRequestEstimatorBindsActualInputAndPrice(t *testing.T) {
	_, ctx, reservation, _ := meterFixture(t)
	pricing, err := optimization.NewTokenPricing(tokenPrice())
	if err != nil {
		t.Fatal(err)
	}
	reservation.PricingRevision = "price-1"
	config := optimization.RequestEstimatorConfig{Scope: modelConfig(ctx, reservation).Scope, Reservation: reservation, Model: "fixture", ModelRevision: "model-1", MaxRequestBytes: 4096}
	estimator, err := optimization.NewRequestEstimator(inputBound(func(_ context.Context, req *llm.Request) (optimization.InputTokenBound, error) {
		if len(req.Tools) != 1 || req.Messages[0].Content != "hello" {
			t.Fatal("counter missed actual request")
		}
		req.Messages[0].Content = "mutation"
		return optimization.InputTokenBound{Model: "fixture", ModelRevision: "model-1", Tokens: 100, Bounded: true}, nil
	}), pricing, config)
	if err != nil {
		t.Fatal(err)
	}
	req := &llm.Request{Model: "fixture", MaxTokens: 10, Messages: []llm.Message{{Content: "hello"}}, Tools: []llm.Tool{{Name: "read"}}}
	got, err := estimator.Estimate(ctx, req)
	if err != nil || got.MaxTokens != 110 || got.MaxUSDMicros != 257 || got.CallKey != reservation.CallKey || got.PricingRevision != "price-1" || req.Messages[0].Content != "hello" {
		t.Fatalf("estimate: %+v %v", got, err)
	}
}

func TestRequestEstimatorRejectsUncertainOrOverflowingInput(t *testing.T) {
	for _, mode := range []string{"unknown", "negative", "overflow", "model", "revision", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			_, ctx, reservation, _ := meterFixture(t)
			bound := optimization.InputTokenBound{Model: "fixture", ModelRevision: "1", Tokens: 10, Bounded: true}
			switch mode {
			case "unknown":
				bound.Bounded = false
			case "negative":
				bound.Tokens = -1
			case "overflow":
				bound.Tokens = math.MaxInt64
			case "model":
				bound.Model = "other"
			case "revision":
				bound.ModelRevision = "other"
			}
			estimator, err := optimization.NewRequestEstimator(inputBound(func(context.Context, *llm.Request) (optimization.InputTokenBound, error) { return bound, nil }), nil, optimization.RequestEstimatorConfig{Scope: modelConfig(ctx, reservation).Scope, Reservation: reservation, Model: "fixture", ModelRevision: "1", TokenOnly: true, MaxRequestBytes: 4096})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := estimator.Estimate(ctx, &llm.Request{Model: "fixture", MaxTokens: 10})
			if err == nil || got.MaxTokens != 0 {
				t.Fatalf("unsafe estimate: %+v %v", got, err)
			}
			if mode == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}
