package storetest

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func leases(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("StaleLeaseCannotCommit", func(t *testing.T) {
		var nanos atomic.Int64
		nanos.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
		s := factory(t, func() time.Time { return time.Unix(0, nanos.Load()).UTC() })
		bundle := Fixture(t, "lease")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		if err := s.Transact(ctx, func(tx store.Tx) error {
			if err := tx.CreateExperiment(bundle); err != nil {
				return err
			}
			return tx.CommitTransition(optimization.Transition{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 1, NextState: optimization.StateQueued, IdempotencyKey: "start", Actor: bundle.Experiment.Spec.Requester})
		}); err != nil {
			t.Fatal(err)
		}
		claim := func(owner string) (optimization.WorkLease, error) {
			var lease optimization.WorkLease
			err := s.Transact(ctx, func(tx store.Tx) error {
				var err error
				lease, err = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: bundle.Experiment.ID, Owner: owner, Duration: 30 * time.Second})
				return err
			})
			return lease, err
		}
		first, err := claim("one")
		if err != nil {
			t.Fatal(err)
		}
		again, err := claim("one")
		if err != nil || again != first {
			t.Fatalf("same live claim changed: %+v %v", again, err)
		}
		if _, err = claim("two"); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("live claim stolen: %v", err)
		}
		nanos.Add(int64(31 * time.Second))
		second, err := claim("two")
		if err != nil {
			t.Fatal(err)
		}
		if second.Generation != first.Generation+1 {
			t.Fatal("takeover reused fencing generation")
		}
		current, err := s.GetExperiment(ctx, bundle.Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		change := optimization.Transition{ExperimentID: current.ID, ExpectedRevision: current.Revision, NextState: optimization.StateRunning, NextPhase: optimization.PhasePropose, Lease: first, IdempotencyKey: "old-worker", Actor: bundle.Experiment.Spec.Requester}
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("stale worker committed despite current revision: %v", err)
		}
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.RenewLease(first) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("stale worker renewed: %v", err)
		}
		change.Lease = second
		change.IdempotencyKey = "new-worker"
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); err != nil {
			t.Fatal(err)
		}
		current, err = s.GetExperiment(ctx, current.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Phase != optimization.PhasePropose || current.Round != 1 {
			t.Fatalf("phase did not advance: %+v", current)
		}
		second.ExpiresAt = second.ExpiresAt.Add(5 * time.Second)
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.RenewLease(second) }); err != nil {
			t.Fatal(err)
		}
		renewed, err := s.GetExperiment(ctx, current.ID)
		if err != nil {
			t.Fatal(err)
		}
		if renewed.Revision != current.Revision || !renewed.Lease.ExpiresAt.Equal(second.ExpiresAt) {
			t.Fatal("heartbeat changed phase revision or lost expiry")
		}
		nanos.Add(int64(time.Minute))
		change.ExpectedRevision = current.Revision
		change.NextPhase = optimization.PhaseProbe
		change.IdempotencyKey = "expired-worker"
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("expired worker committed: %v", err)
		}
	})
	t.Run("InitialWorkStateRejected", func(t *testing.T) {
		for name, mutate := range map[string]func(*optimization.Experiment){
			"owner":    func(e *optimization.Experiment) { e.Lease.Owner = "injected" },
			"expiry":   func(e *optimization.Experiment) { e.Lease.ExpiresAt = time.Now() },
			"phase":    func(e *optimization.Experiment) { e.Phase = optimization.PhasePropose },
			"revision": func(e *optimization.Experiment) { e.Revision = 5 },
		} {
			t.Run(name, func(t *testing.T) {
				s := factory(t, time.Now)
				bundle := Fixture(t, name)
				mutate(&bundle.Experiment)
				if err := s.Transact(scoped(bundle.Experiment.Spec.Scope), func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); !errors.Is(err, optimization.ErrInvalidSpec) {
					t.Fatalf("injected state accepted: %v", err)
				}
			})
		}
	})
}
