package storetest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

type callBeginner interface {
	BeginUsageCall(optimization.UsageReservation, string) (optimization.Reservation, error)
}

func beginUsage(ctx context.Context, s store.Store, request optimization.UsageReservation, digest string) error {
	return s.Transact(ctx, func(tx store.Tx) error {
		beginner, ok := tx.(callBeginner)
		if !ok {
			return errors.New("missing durable call dispatch boundary")
		}
		_, err := beginner.BeginUsageCall(request, digest)
		return err
	})
}

func dispatch(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("UsageCallStartsOnce", func(t *testing.T) {
		s, ctx, experiment, lease := runningBudget(t, factory, optimization.Budget{MaxTokens: 100, TokenOnly: true})
		request := reservation(experiment, lease, "external-call", 60)
		digest := strings.Repeat("a", 64)
		results := make(chan error, 2)
		var group sync.WaitGroup
		for range 2 {
			group.Go(func() { results <- beginUsage(ctx, s, request, digest) })
		}
		group.Wait()
		close(results)
		wins := 0
		for err := range results {
			if err == nil {
				wins++
			} else if !errors.Is(err, optimization.ErrUnknownAttempt) && !errors.Is(err, optimization.ErrConflict) {
				t.Fatal(err)
			}
		}
		if wins != 1 {
			t.Fatalf("external call admitted %d times", wins)
		}
		if err := beginUsage(ctx, s, request, digest); !errors.Is(err, optimization.ErrUnknownAttempt) {
			t.Fatalf("retry admitted: %v", err)
		}
		if err := beginUsage(ctx, s, request, strings.Repeat("b", 64)); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("different payload reused call key: %v", err)
		}
		if err := beginUsage(ctx, s, reservation(experiment, lease, "excess", 41), digest); !errors.Is(err, optimization.ErrBudgetExceeded) {
			t.Fatalf("duplicate dispatch freed debit: %v", err)
		}
		events, err := s.ListEvents(ctx, experiment, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		started := 0
		for _, event := range events.Items {
			if event.Type == "usage_call_started" {
				started++
			}
		}
		if started != 1 {
			t.Fatalf("start audit count=%d", started)
		}
	})
	t.Run("UsageCallRollbackAndFences", func(t *testing.T) {
		now := time.Now().UTC()
		s, ctx, experiment, lease := runningBudgetWithClock(t, factory, optimization.Budget{MaxTokens: 100, TokenOnly: true}, func() time.Time { return now })
		request := reservation(experiment, lease, "external-call", 60)
		digest := strings.Repeat("a", 64)
		abort := errors.New("abort call boundary")
		if err := s.Transact(ctx, func(tx store.Tx) error {
			beginner, ok := tx.(callBeginner)
			if !ok {
				return errors.New("missing durable call dispatch boundary")
			}
			if _, beginErr := beginner.BeginUsageCall(request, digest); beginErr != nil {
				return beginErr
			}
			return abort
		}); !errors.Is(err, abort) {
			t.Fatal(err)
		}
		if _, err := s.GetReservation(ctx, experiment, request.CallKey); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("aborted dispatch debited budget: %v", err)
		}
		for _, badDigest := range []string{"", strings.Repeat("A", 64), "not-a-digest"} {
			if err := beginUsage(ctx, s, request, badDigest); !errors.Is(err, optimization.ErrInvalidSpec) {
				t.Fatalf("invalid request binding accepted: %v", err)
			}
		}
		stale := request
		stale.Lease.Generation++
		if err := beginUsage(ctx, s, stale, digest); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("stale worker started call: %v", err)
		}
		foreign := scoped(Fixture(t, "foreign-call").Experiment.Spec.Scope)
		if err := beginUsage(foreign, s, request, digest); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("foreign worker started call: %v", err)
		}
		if err := beginUsage(ctx, s, request, digest); err != nil {
			t.Fatal(err)
		}
		now = lease.ExpiresAt
		var replacement optimization.WorkLease
		if err := s.Transact(ctx, func(tx store.Tx) error {
			var claimErr error
			replacement, claimErr = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: experiment, Owner: "replacement", Duration: time.Minute})
			return claimErr
		}); err != nil {
			t.Fatal(err)
		}
		request.Lease = replacement
		request.ExpiresAt = replacement.ExpiresAt
		// A takeover cannot extend the original call's immutable expiry.
		if err := beginUsage(ctx, s, request, digest); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("takeover rewrote dispatch: %v", err)
		}
	})
	t.Run("UsageCallRejectsSettledReservation", func(t *testing.T) {
		s, ctx, experiment, lease := runningBudget(t, factory, optimization.Budget{MaxTokens: 100, TokenOnly: true})
		request := reservation(experiment, lease, "imported-call", 60)
		reserve(ctx, t, s, request)
		settlement := optimization.UsageSettlement{ExperimentID: experiment, AttemptID: request.AttemptID, CallKey: request.CallKey, Receipt: "provider/receipt", Usage: optimization.Usage{Tokens: 10, TokenQuality: optimization.UsageKnown}, Complete: true, Lease: lease}
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.Settle(settlement) }); err != nil {
			t.Fatal(err)
		}
		if err := beginUsage(ctx, s, request, strings.Repeat("a", 64)); !errors.Is(err, optimization.ErrUnknownAttempt) {
			t.Fatalf("settled call restarted: %v", err)
		}
		request.AttemptID = id.New(optimization.PrefixAttempt)
		if err := beginUsage(ctx, s, request, strings.Repeat("a", 64)); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("different attempt reused debit: %v", err)
		}
	})
	t.Run("UsageCallOverrunStopsReservedCall", func(t *testing.T) {
		s, ctx, experiment, lease := runningBudget(t, factory, optimization.Budget{MaxTokens: 100, TokenOnly: true})
		first := reservation(experiment, lease, "first", 60)
		second := reservation(experiment, lease, "second", 40)
		reserve(ctx, t, s, first)
		reserve(ctx, t, s, second)
		settlement := optimization.UsageSettlement{ExperimentID: experiment, AttemptID: first.AttemptID, CallKey: first.CallKey, Receipt: "provider/overrun", Usage: optimization.Usage{Tokens: 120, TokenQuality: optimization.UsageKnown}, Complete: true, Lease: lease}
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.Settle(settlement) }); err != nil {
			t.Fatal(err)
		}
		if err := beginUsage(ctx, s, second, strings.Repeat("a", 64)); !errors.Is(err, optimization.ErrBudgetExceeded) {
			t.Fatalf("reserved call ignored later overrun: %v", err)
		}
		saved, err := s.GetReservation(ctx, experiment, second.CallKey)
		if err != nil || saved.Call != nil {
			t.Fatalf("overrun still marked dispatch: %+v %v", saved, err)
		}
	})
	t.Run("UsageCallExpiredDispatchRequiresReconciliation", func(t *testing.T) {
		now := time.Now().UTC()
		s, ctx, experiment, lease := runningBudgetWithClock(t, factory, optimization.Budget{MaxTokens: 100, TokenOnly: true}, func() time.Time { return now })
		request := reservation(experiment, lease, "expired-dispatch", 60)
		request.ExpiresAt = now.Add(time.Second)
		digest := strings.Repeat("a", 64)
		if err := beginUsage(ctx, s, request, digest); err != nil {
			t.Fatal(err)
		}
		now = now.Add(2 * time.Second)
		if err := beginUsage(ctx, s, request, digest); !errors.Is(err, optimization.ErrUnknownAttempt) {
			t.Fatalf("expired dispatch lost reconciliation identity: %v", err)
		}
		request.CallKey = "expired-undispatched"
		if err := beginUsage(ctx, s, request, digest); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatalf("expired new call dispatched: %v", err)
		}
	})
}
