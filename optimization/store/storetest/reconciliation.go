package storetest

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func reconciliation(t *testing.T, factory Factory) {
	t.Helper()
	for _, cancelled := range []bool{false, true} {
		name := "Expired"
		if cancelled {
			name = "Cancelled"
		}
		t.Run("LateUsageReceipt/"+name, func(t *testing.T) {
			now := time.Now().UTC()
			s, ctx, experiment, lease := runningBudgetWithClock(t, factory, optimization.Budget{MaxTokens: 100, TokenOnly: true}, func() time.Time { return now })
			req := reservation(experiment, lease, "late", 60)
			digest := strings.Repeat("a", 64)
			if err := beginUsage(ctx, s, req, digest); err != nil {
				t.Fatal(err)
			}
			saved, err := s.GetReservation(ctx, experiment, req.CallKey)
			if err != nil {
				t.Fatal(err)
			}
			wantState := optimization.StateRunning
			if cancelled {
				current, getErr := s.GetExperiment(ctx, experiment)
				if getErr != nil {
					t.Fatal(getErr)
				}
				if err = s.Transact(ctx, func(tx store.Tx) error {
					return tx.CommitTransition(optimization.Transition{ExperimentID: experiment, ExpectedRevision: current.Revision, NextState: optimization.StateCancelled, IdempotencyKey: "cancel-before-receipt", Actor: current.Spec.Requester})
				}); err != nil {
					t.Fatal(err)
				}
				wantState = optimization.StateCancelled
			}
			now = now.Add(time.Hour)
			settlement := optimization.UsageSettlement{ExperimentID: experiment, AttemptID: req.AttemptID, CallKey: req.CallKey, Receipt: "verified-late", Usage: optimization.Usage{Tokens: 20, TokenQuality: optimization.UsageKnown, Receipt: "verified-late"}, Complete: true}
			if err = s.Transact(ctx, func(tx store.Tx) error { return tx.Settle(settlement) }); !errors.Is(err, optimization.ErrConflict) {
				t.Fatalf("expired worker settlement: %v", err)
			}
			recovery := optimization.UsageReconciliation{Settlement: settlement, RequestDigest: digest, ExpectedRevision: saved.Revision, Actor: optimization.IdentityRef{Subject: "billing-recovery"}}
			apply := func(value optimization.UsageReconciliation) error {
				return s.Transact(ctx, func(tx store.Tx) error {
					return tx.ReconcileUsage(value)
				})
			}
			for _, mode := range []string{"digest", "attempt", "unknown", "revision", "actor"} {
				bad := recovery
				switch mode {
				case "digest":
					bad.RequestDigest = strings.Repeat("b", 64)
				case "attempt":
					bad.Settlement.AttemptID = experiment
				case "unknown":
					bad.Settlement.Complete = false
				case "revision":
					bad.ExpectedRevision++
				case "actor":
					bad.Actor.Subject = ""
				}
				if err = apply(bad); err == nil {
					t.Fatalf("accepted invalid %s recovery", mode)
				}
			}
			for range 2 {
				if err = apply(recovery); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.GetReservation(ctx, experiment, req.CallKey)
			if err != nil || got.DebitedTokens != 20 || len(got.History) != 1 || got.State != optimization.ReservationSettled || got.Call.RequestDigest != digest {
				t.Fatalf("late receipt: %+v %v", got, err)
			}
			stored, err := s.GetExperiment(ctx, experiment)
			if err != nil || stored.Lease != lease || stored.State != wantState {
				t.Fatalf("recovery changed execution authority: %+v %v", stored, err)
			}
			if err = beginUsage(ctx, s, req, digest); err == nil {
				t.Fatal("recovery granted replay")
			}
		})
	}
}
