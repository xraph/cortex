package optimization_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization"
)

type usageReceiptProvider func(context.Context, optimization.Reservation) (optimization.VerifiedUsageReceipt, error)

func (f usageReceiptProvider) Recover(ctx context.Context, value optimization.Reservation) (optimization.VerifiedUsageReceipt, error) {
	return f(ctx, value)
}

func TestUsageRecoveryRechecksGrantsAndNeverDispatches(t *testing.T) {
	for _, mode := range []string{"success", "wrong-digest", "revoked", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx, req, _ := meterFixture(t)
			if _, err := allowModelMeter(t, s).BeginCall(ctx, req, strings.Repeat("a", 64)); err != nil {
				t.Fatal(err)
			}
			revoked := false
			auth := meterAuthorizer(func(context.Context, optimization.Action, optimization.Resource) error {
				if revoked {
					return optimization.ErrPolicyDenied
				}
				return nil
			})
			calls := 0
			provider := usageReceiptProvider(func(_ context.Context, r optimization.Reservation) (optimization.VerifiedUsageReceipt, error) {
				calls++
				if mode == "revoked" {
					revoked = true
				}
				if mode == "unknown" {
					return optimization.VerifiedUsageReceipt{}, optimization.ErrUnknownAttempt
				}
				digest := r.Call.RequestDigest
				if mode == "wrong-digest" {
					digest = strings.Repeat("b", 64)
				}
				return optimization.VerifiedUsageReceipt{RequestDigest: digest, Settlement: optimization.UsageSettlement{ExperimentID: req.ExperimentID, AttemptID: req.AttemptID, CallKey: req.CallKey, Receipt: "provider/late", Usage: optimization.Usage{Tokens: 20, TokenQuality: optimization.UsageKnown, Receipt: "provider/late"}, Complete: true}}, nil
			})
			recovery, err := optimization.NewUsageRecovery(s, auth, meterResolver(func(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
				return ctx, nil
			}), provider)
			if err != nil {
				t.Fatal(err)
			}
			err = recovery.Recover(ctx, req.ExperimentID, req.CallKey, optimization.IdentityRef{Subject: "billing"})
			if mode == "success" && err != nil {
				t.Fatal(err)
			}
			if mode != "success" && err == nil {
				t.Fatal("invalid recovery accepted")
			}
			saved, getErr := s.GetReservation(ctx, req.ExperimentID, req.CallKey)
			want := int64(60)
			if mode == "success" {
				want = 20
			}
			if getErr != nil || saved.DebitedTokens != want || calls != 1 {
				t.Fatalf("recovery: %+v %v calls=%d", saved, getErr, calls)
			}
			if mode == "success" {
				if err = recovery.Recover(ctx, req.ExperimentID, req.CallKey, optimization.IdentityRef{Subject: "billing"}); err != nil || calls != 1 {
					t.Fatalf("settled recovery called provider: %v %d", err, calls)
				}
			}
			if _, err = allowModelMeter(t, s).BeginCall(ctx, req, strings.Repeat("a", 64)); !errors.Is(err, optimization.ErrUnknownAttempt) {
				t.Fatalf("replayed: %v", err)
			}
		})
	}
}
