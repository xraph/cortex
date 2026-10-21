package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

// IndependentScopes checks that one store instance allows another scope to
// commit while a transaction is held. Use it for database backends that support
// independent writers; SQLite deliberately has a database-wide write lock.
func IndependentScopes(t *testing.T, s store.Store) {
	t.Helper()
	a, b := Fixture(t, "shared-instance-a"), Fixture(t, "shared-instance-b")
	deadline, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctxA := cortex.WithScope(deadline, a.Experiment.Spec.Scope)
	ctxB := cortex.WithScope(deadline, b.Experiment.Spec.Scope)
	entered, release := make(chan struct{}), make(chan struct{})
	first, second := make(chan error, 1), make(chan error, 1)
	go func() {
		first <- s.Transact(ctxA, func(tx store.Tx) error {
			close(entered)
			<-release
			return tx.CreateExperiment(a)
		})
	}()
	select {
	case <-entered:
	case err := <-first:
		t.Fatalf("first operation: %v", err)
	}
	go func() { second <- s.Transact(ctxB, func(tx store.Tx) error { return tx.CreateExperiment(b) }) }()
	select {
	case err := <-second:
		close(release)
		if firstErr := <-first; firstErr != nil {
			t.Error(firstErr)
		}
		if err != nil {
			t.Fatal(err)
		}
	case <-deadline.Done():
		close(release)
		<-first
		<-second
		t.Fatal("independent scope waited for unrelated transaction")
	}
	for _, bundle := range []optimization.ExperimentBundle{a, b} {
		ctx := cortex.WithScope(deadline, bundle.Experiment.Spec.Scope)
		got, err := s.GetExperiment(ctx, bundle.Experiment.ID)
		if err != nil || got.ID != bundle.Experiment.ID {
			t.Fatalf("committed scope missing: %v, %v", got.ID, err)
		}
	}
}
