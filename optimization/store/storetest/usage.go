package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func accounting(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("ReservationRace", func(t *testing.T) {
		s, ctx, experiment, lease := runningBudget(t, factory, optimization.Budget{MaxTokens: 100, TokenOnly: true})
		results := make(chan error, 2)
		var group sync.WaitGroup
		for i := range 2 {
			group.Add(1)
			go func() {
				defer group.Done()
				req := reservation(experiment, lease, fmt.Sprintf("call-%d", i), 60)
				results <- s.Transact(ctx, func(tx store.Tx) error { _, reserveErr := tx.Reserve(req); return reserveErr })
			}()
		}
		group.Wait()
		close(results)
		wins, refused := 0, 0
		for err := range results {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, optimization.ErrBudgetExceeded), errors.Is(err, optimization.ErrConflict):
				refused++
			default:
				t.Fatal(err)
			}
		}
		if wins != 1 || refused != 1 {
			t.Fatalf("100-token budget admitted two 60-token calls: %d/%d", wins, refused)
		}
		// Optimistic backends can refuse the losing call at the database fence
		// before its callback reaches the budget check. Inspect committed state
		// and attempt a fresh reservation to prove the budget was still enforced.
		stored := 0
		for i := range 2 {
			value, err := s.GetReservation(ctx, experiment, fmt.Sprintf("call-%d", i))
			if errors.Is(err, optimization.ErrNotFound) {
				continue
			}
			if err != nil || value.Request.MaxTokens != 60 || value.DebitedTokens != 60 || value.State != optimization.ReservationHeld {
				t.Fatalf("committed reservation: %+v, %v", value, err)
			}
			stored++
		}
		if stored != 1 {
			t.Fatalf("stored %d competing reservations, want one", stored)
		}
		if err := s.Transact(ctx, func(tx store.Tx) error {
			_, reserveErr := tx.Reserve(reservation(experiment, lease, "after-race", 41))
			return reserveErr
		}); !errors.Is(err, optimization.ErrBudgetExceeded) {
			t.Fatalf("reservation race lost its debit: %v", err)
		}
	})
	t.Run("DuplicateReceiptSettlesOnce", func(t *testing.T) {
		s, ctx, experiment, lease := runningBudget(t, factory, optimization.Budget{MaxTokens: 100, TokenOnly: true})
		req := reservation(experiment, lease, "call", 60)
		reserve(ctx, t, s, req)
		reserve(ctx, t, s, req)
		changed := req
		changed.MaxTokens = 20
		if err := s.Transact(ctx, func(tx store.Tx) error { _, reserveErr := tx.Reserve(changed); return reserveErr }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("call key changed bounds: %v", err)
		}
		settlement := optimization.UsageSettlement{ExperimentID: experiment, AttemptID: req.AttemptID, CallKey: req.CallKey, Receipt: "provider/call", Usage: optimization.Usage{Tokens: 20, TokenQuality: optimization.UsageKnown}, Complete: true, Lease: lease}
		for range 2 {
			if err := s.Transact(ctx, func(tx store.Tx) error { return tx.Settle(settlement) }); err != nil {
				t.Fatal(err)
			}
		}
		stored, err := s.GetReservation(ctx, experiment, req.CallKey)
		if err != nil {
			t.Fatal(err)
		}
		if stored.State != optimization.ReservationSettled || stored.DebitedTokens != 20 || stored.Settlement.Usage.Tokens != 20 {
			t.Fatalf("wrong settled debit: %+v", stored)
		}
		reserve(ctx, t, s, reservation(experiment, lease, "remaining", 80))
		if err = s.Transact(ctx, func(tx store.Tx) error {
			_, reserveErr := tx.Reserve(reservation(experiment, lease, "over", 1))
			return reserveErr
		}); !errors.Is(err, optimization.ErrBudgetExceeded) {
			t.Fatalf("duplicate receipt freed extra balance: %v", err)
		}
		settlement.Usage.Tokens = 10
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.Settle(settlement) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("receipt payload changed: %v", err)
		}
	})
	t.Run("UnknownUsageKeepsReservation", func(t *testing.T) {
		s, ctx, experiment, lease := runningBudget(t, factory, optimization.Budget{MaxTokens: 100, TokenOnly: true})
		req := reservation(experiment, lease, "uncertain", 60)
		reserve(ctx, t, s, req)
		unknown := optimization.UsageSettlement{ExperimentID: experiment, AttemptID: req.AttemptID, CallKey: req.CallKey, Receipt: "observation/timeout", Lease: lease}
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.Settle(unknown) }); err != nil {
			t.Fatal(err)
		}
		if err := s.Transact(ctx, func(tx store.Tx) error {
			_, reserveErr := tx.Reserve(reservation(experiment, lease, "too-much", 50))
			return reserveErr
		}); !errors.Is(err, optimization.ErrBudgetExceeded) {
			t.Fatalf("unknown usage released debit: %v", err)
		}
		known := unknown
		known.Receipt = "provider/recovered"
		known.Complete = true
		known.Usage = optimization.Usage{Tokens: 20, TokenQuality: optimization.UsageKnown}
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.Settle(known) }); err != nil {
			t.Fatal(err)
		}
		// A delayed retry of the timeout acknowledgment must not undo reconciliation.
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.Settle(unknown) }); err != nil {
			t.Fatal(err)
		}
		reserve(ctx, t, s, reservation(experiment, lease, "remaining", 80))
		stored, err := s.GetReservation(ctx, experiment, req.CallKey)
		if err != nil {
			t.Fatal(err)
		}
		if stored.State != optimization.ReservationSettled || len(stored.History) != 2 {
			t.Fatalf("reconciliation history lost: %+v", stored)
		}
	})
	t.Run("MoneyBoundsAndUnknownPricing", func(t *testing.T) {
		s, ctx, experiment, lease := runningBudget(t, factory, optimization.Budget{MaxTokens: 100, MaxUSDMicros: 100, PricingRevision: "prices-1"})
		req := reservation(experiment, lease, "priced", 10)
		req.MaxUSDMicros = 60
		req.PricingRevision = "prices-1"
		reserve(ctx, t, s, req)
		bad := reservation(experiment, lease, "wrong-price", 10)
		bad.MaxUSDMicros = 1
		bad.PricingRevision = "prices-2"
		if err := s.Transact(ctx, func(tx store.Tx) error { _, reserveErr := tx.Reserve(bad); return reserveErr }); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatalf("unfrozen price accepted: %v", err)
		}
		partial := optimization.UsageSettlement{ExperimentID: experiment, AttemptID: req.AttemptID, CallKey: req.CallKey, Receipt: "provider/no-cost", Usage: optimization.Usage{Tokens: 5, TokenQuality: optimization.UsageKnown}, Complete: true, Lease: lease}
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.Settle(partial) }); err != nil {
			t.Fatal(err)
		}
		more := reservation(experiment, lease, "money-over", 1)
		more.MaxUSDMicros = 50
		more.PricingRevision = "prices-1"
		if err := s.Transact(ctx, func(tx store.Tx) error { _, reserveErr := tx.Reserve(more); return reserveErr }); !errors.Is(err, optimization.ErrBudgetExceeded) {
			t.Fatalf("missing money usage became zero: %v", err)
		}
		stored, err := s.GetReservation(ctx, experiment, req.CallKey)
		if err != nil {
			t.Fatal(err)
		}
		if stored.DebitedTokens != 5 || stored.DebitedUSDMicros != 60 || stored.State != optimization.ReservationUnknown {
			t.Fatalf("separate accounting quality lost: %+v", stored)
		}
	})
	t.Run("OverrunIsRetained", func(t *testing.T) {
		s, ctx, experiment, lease := runningBudget(t, factory, optimization.Budget{MaxTokens: 100, TokenOnly: true})
		req := reservation(experiment, lease, "overrun", 60)
		reserve(ctx, t, s, req)
		settlement := optimization.UsageSettlement{ExperimentID: experiment, AttemptID: req.AttemptID, CallKey: req.CallKey, Receipt: "provider/overrun", Usage: optimization.Usage{Tokens: 120, TokenQuality: optimization.UsageKnown}, Complete: true, Lease: lease}
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.Settle(settlement) }); err != nil {
			t.Fatal(err)
		}
		stored, err := s.GetReservation(ctx, experiment, req.CallKey)
		if err != nil {
			t.Fatal(err)
		}
		if stored.DebitedTokens != 120 {
			t.Fatal("overrun was clamped out of accounting")
		}
		if err = s.Transact(ctx, func(tx store.Tx) error {
			_, reserveErr := tx.Reserve(reservation(experiment, lease, "after-overrun", 1))
			return reserveErr
		}); !errors.Is(err, optimization.ErrBudgetExceeded) {
			t.Fatalf("overrun allowed more dispatch: %v", err)
		}
	})
	t.Run("ReservationRollback", func(t *testing.T) {
		s, ctx, experiment, lease := runningBudget(t, factory, optimization.Budget{MaxTokens: 100, TokenOnly: true})
		req := reservation(experiment, lease, "rollback", 60)
		abort := errors.New("abort")
		if err := s.Transact(ctx, func(tx store.Tx) error {
			if _, reserveErr := tx.Reserve(req); reserveErr != nil {
				return reserveErr
			}
			return abort
		}); !errors.Is(err, abort) {
			t.Fatal(err)
		}
		if _, err := s.GetReservation(ctx, experiment, req.CallKey); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("reservation survived rollback: %v", err)
		}
		reserve(ctx, t, s, reservation(experiment, lease, "whole-budget", 100))
	})
	t.Run("UsageAuditLinksCall", func(t *testing.T) {
		s, ctx, experiment, lease := runningBudget(t, factory, optimization.Budget{MaxTokens: 100, TokenOnly: true})
		req := reservation(experiment, lease, "audited-call", 60)
		reserve(ctx, t, s, req)
		settlement := optimization.UsageSettlement{ExperimentID: experiment, AttemptID: req.AttemptID, CallKey: req.CallKey, Receipt: "provider/audited-call", Usage: optimization.Usage{Tokens: 20, TokenQuality: optimization.UsageKnown}, Complete: true, Lease: lease}
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.Settle(settlement) }); err != nil {
			t.Fatal(err)
		}
		events, err := s.ListEvents(ctx, experiment, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, event := range events.Items {
			if event.Type != "usage_reserved" && event.Type != "usage_settled" {
				continue
			}
			seen++
			if !slices.Contains(event.Resources, req.AttemptID) {
				t.Fatal("accounting event omitted external attempt identity")
			}
			var payload struct {
				CallKey string `json:"call_key"`
				Tokens  int64  `json:"tokens_debited"`
			}
			if err = json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			want := int64(60)
			if event.Type == "usage_settled" {
				want = 20
			}
			if payload.CallKey != req.CallKey || payload.Tokens != want {
				t.Fatalf("untraceable debit event: %+v", payload)
			}
		}
		if seen != 2 {
			t.Fatalf("missing accounting events: %d", seen)
		}
	})
	t.Run("ExpiredReservationKeepsDebit", func(t *testing.T) {
		var nanos atomic.Int64
		nanos.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
		s, ctx, experiment, lease := runningBudgetWithClock(t, factory, optimization.Budget{MaxTokens: 100, TokenOnly: true}, func() time.Time { return time.Unix(0, nanos.Load()).UTC() })
		req := reservation(experiment, lease, "expired-call", 60)
		req.ExpiresAt = time.Unix(0, nanos.Load()).Add(time.Second)
		reserve(ctx, t, s, req)
		nanos.Add(int64(2 * time.Second))
		if err := s.Transact(ctx, func(tx store.Tx) error {
			_, reserveErr := tx.Reserve(reservation(experiment, lease, "after-expiry", 50))
			return reserveErr
		}); !errors.Is(err, optimization.ErrBudgetExceeded) {
			t.Fatalf("expiry refunded an unresolved call: %v", err)
		}
		estimated := optimization.UsageSettlement{ExperimentID: experiment, AttemptID: req.AttemptID, CallKey: req.CallKey, Receipt: "estimate", Usage: optimization.Usage{Tokens: 10, TokenQuality: optimization.UsageEstimated}, Complete: true, Lease: lease}
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.Settle(estimated) }); err != nil {
			t.Fatal(err)
		}
		stored, err := s.GetReservation(ctx, experiment, req.CallKey)
		if err != nil {
			t.Fatal(err)
		}
		if stored.DebitedTokens != 60 || stored.State != optimization.ReservationUnknown {
			t.Fatalf("estimate released conservative debit: %+v", stored)
		}
	})
	t.Run("SettlementFencesAndRollback", func(t *testing.T) {
		s, ctx, experiment, lease := runningBudget(t, factory, optimization.Budget{MaxTokens: 100, TokenOnly: true})
		req := reservation(experiment, lease, "fenced-call", 60)
		reserve(ctx, t, s, req)
		settlement := optimization.UsageSettlement{ExperimentID: experiment, AttemptID: req.AttemptID, CallKey: req.CallKey, Receipt: "provider/fenced", Usage: optimization.Usage{Tokens: 20, TokenQuality: optimization.UsageKnown}, Complete: true, Lease: lease}
		for _, mutate := range []func(*optimization.UsageSettlement){func(s *optimization.UsageSettlement) { s.Lease.Generation++ }, func(s *optimization.UsageSettlement) { s.AttemptID = id.New(optimization.PrefixAttempt) }} {
			bad := settlement
			mutate(&bad)
			if err := s.Transact(ctx, func(tx store.Tx) error { return tx.Settle(bad) }); !errors.Is(err, optimization.ErrConflict) {
				t.Fatalf("foreign authority settled call: %v", err)
			}
		}
		foreign := scoped(Fixture(t, "foreign-budget").Experiment.Spec.Scope)
		if err := s.Transact(foreign, func(tx store.Tx) error { return tx.Settle(settlement) }); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("foreign scope settled call: %v", err)
		}
		before, err := s.ListEvents(ctx, experiment, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		abort := errors.New("abort settlement")
		if err = s.Transact(ctx, func(tx store.Tx) error {
			if settleErr := tx.Settle(settlement); settleErr != nil {
				return settleErr
			}
			return abort
		}); !errors.Is(err, abort) {
			t.Fatal(err)
		}
		stored, err := s.GetReservation(ctx, experiment, req.CallKey)
		if err != nil {
			t.Fatal(err)
		}
		after, err := s.ListEvents(ctx, experiment, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if stored.DebitedTokens != 60 || stored.State != optimization.ReservationHeld || len(stored.History) != 0 || len(before.Items) != len(after.Items) {
			t.Fatal("settlement or audit survived rollback")
		}
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.Settle(settlement) }); err != nil {
			t.Fatal(err)
		}
		other := reservation(experiment, lease, "second-call", 60)
		reserve(ctx, t, s, other)
		settlement.CallKey, settlement.AttemptID = other.CallKey, other.AttemptID
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.Settle(settlement) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("receipt reused for another call: %v", err)
		}
	})
}

func runningBudget(t *testing.T, factory Factory, budget optimization.Budget) (store.Store, context.Context, id.ID, optimization.WorkLease) {
	t.Helper()
	return runningBudgetWithClock(t, factory, budget, time.Now)
}

func runningBudgetWithClock(t *testing.T, factory Factory, budget optimization.Budget, now func() time.Time) (store.Store, context.Context, id.ID, optimization.WorkLease) {
	t.Helper()
	s := factory(t, now)
	bundle := Fixture(t, "budget")
	bundle.Experiment.Spec.Budget = budget
	ctx := scoped(bundle.Experiment.Spec.Scope)
	var lease optimization.WorkLease
	if err := s.Transact(ctx, func(tx store.Tx) error {
		if err := tx.CreateExperiment(bundle); err != nil {
			return err
		}
		if err := tx.CommitTransition(optimization.Transition{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 1, NextState: optimization.StateQueued, IdempotencyKey: "start", Actor: bundle.Experiment.Spec.Requester}); err != nil {
			return err
		}
		var err error
		lease, err = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: bundle.Experiment.ID, Owner: "budget-worker", Duration: 5 * time.Minute})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return s, ctx, bundle.Experiment.ID, lease
}

func reservation(experiment id.ID, lease optimization.WorkLease, key string, tokens int64) optimization.UsageReservation {
	return optimization.UsageReservation{ExperimentID: experiment, AttemptID: id.New(optimization.PrefixAttempt), CallKey: key, Operation: optimization.OperationProbe, MaxTokens: tokens, Lease: lease, ExpiresAt: lease.ExpiresAt}
}

func reserve(ctx context.Context, t *testing.T, s store.Store, req optimization.UsageReservation) {
	t.Helper()
	if err := s.Transact(ctx, func(tx store.Tx) error { _, reserveErr := tx.Reserve(req); return reserveErr }); err != nil {
		t.Fatal(err)
	}
}
