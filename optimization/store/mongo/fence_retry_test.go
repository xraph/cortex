package mongo

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestScopeFenceWaitsForContendedTransaction(t *testing.T) {
	uri := testDatabase(t, true)
	first, second := New(openDatabase(t, uri)), New(openDatabase(t, uri))
	if err := first.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	bundle := storetest.Fixture(t, "contended-command")
	deadline, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ctx := cortex.WithScope(deadline, bundle.Experiment.Spec.Scope)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- first.Transact(ctx, func(tx store.Tx) error {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			return tx.CreateExperiment(bundle)
		})
	}()
	select {
	case <-entered:
	case err := <-firstDone:
		t.Fatalf("initial fence: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancelled, stop := context.WithTimeout(ctx, 75*time.Millisecond)
	defer stop()
	cancelledCalls := 0
	if err := second.Transact(cancelled, func(store.Tx) error { cancelledCalls++; return nil }); !errors.Is(err, context.DeadlineExceeded) || cancelledCalls != 0 {
		t.Fatalf("cancelled fence: %v; callbacks: %d", err, cancelledCalls)
	}
	secondDone := make(chan error, 1)
	calls := 0
	go func() {
		secondDone <- second.Transact(ctx, func(store.Tx) error { calls++; return nil })
	}()
	// The previous retry schedule exhausted itself before this holder released.
	select {
	case err := <-secondDone:
		unblock()
		<-firstDone
		t.Fatalf("contender stopped before fence release: %v", err)
	case <-time.After(1200 * time.Millisecond):
	}
	unblock()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil || calls != 1 {
		t.Fatalf("contender: %v; callbacks: %d", err, calls)
	}
	if _, err := second.GetExperiment(ctx, bundle.Experiment.ID); err != nil {
		t.Fatal(err)
	}
	callbackFailure := optimization.ErrConflict
	calls = 0
	if err := second.Transact(ctx, func(store.Tx) error { calls++; return callbackFailure }); !errors.Is(err, callbackFailure) || calls != 1 {
		t.Fatalf("callback retry: %v; callbacks: %d", err, calls)
	}
}
