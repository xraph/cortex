package state

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

// Domain behavior is covered by storetest. This backend exposes only operation
// admission and lifecycle boundaries, without database timing in the assertions.
type admissionBackend struct {
	Backend
	closes atomic.Int32
	loads  atomic.Int32
}

func (b *admissionBackend) Update(_ context.Context, _ string, fn func([]Record) ([]Mutation, error)) error {
	_, err := fn(nil)
	return err
}

func (b *admissionBackend) Load(context.Context, string) ([]Record, error) {
	b.loads.Add(1)
	return nil, nil
}

func (b *admissionBackend) LoadRecord(ctx context.Context, scope string, _ RecordKey) ([]Record, error) {
	return b.Load(ctx, scope)
}

func (b *admissionBackend) Close() error { b.closes.Add(1); return nil }

func operationContext(name string) context.Context {
	return cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: name}}})
}

func holdOperation(t *testing.T, s *Store) func() {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- s.Transact(operationContext("held"), func(store.Tx) error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-finished:
		t.Fatalf("first operation: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("first operation did not start")
	}
	return func() {
		close(release)
		if err := <-finished; err != nil {
			t.Error(err)
		}
	}
}

func TestDurableStoreAdmitsIndependentReadsAndWrites(t *testing.T) {
	for _, operation := range []string{"read", "write"} {
		t.Run(operation, func(t *testing.T) {
			backend := &admissionBackend{}
			s := NewBackend(backend)
			release := holdOperation(t, s)
			finished := make(chan error, 1)
			go func() {
				ctx := operationContext("independent")
				if operation == "write" {
					finished <- s.Transact(ctx, func(store.Tx) error { return nil })
					return
				}
				_, err := s.GetExperiment(ctx, id.New(optimization.PrefixExperiment))
				if errors.Is(err, optimization.ErrNotFound) {
					err = nil
				}
				finished <- err
			}()
			select {
			case err := <-finished:
				release()
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				release()
				<-finished
				t.Fatal("independent operation waited for unrelated transaction")
			}
		})
	}
}

func TestCloseDrainsDurableOperationsAndRefusesNewWork(t *testing.T) {
	backend := &admissionBackend{}
	s := NewBackend(backend)
	release := holdOperation(t, s)
	closing := make(chan error, 1)
	go func() { closing <- s.Close() }()
	select {
	case err := <-closing:
		release()
		t.Fatalf("close did not drain operation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if backend.closes.Load() != 0 {
		t.Error("backend closed with operation in flight")
	}
	release()
	if err := <-closing; err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil || backend.closes.Load() != 1 {
		t.Fatalf("close retry: %v, count %d", err, backend.closes.Load())
	}
	called := false
	if err := s.Transact(operationContext("new"), func(store.Tx) error { called = true; return nil }); !errors.Is(err, optimization.ErrUnavailable) || called {
		t.Fatalf("closed transaction: %v, callback %v", err, called)
	}
	if _, err := s.GetExperiment(operationContext("new"), id.New(optimization.PrefixExperiment)); !errors.Is(err, optimization.ErrUnavailable) || backend.loads.Load() != 0 {
		t.Fatalf("closed read: %v, loads %d", err, backend.loads.Load())
	}
}
