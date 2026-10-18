package memory_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T, now func() time.Time) store.Store {
		t.Helper()
		s, err := memory.New(memory.WithClock(now))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
		return s
	})
}

func TestClockOwnsCreationTime(t *testing.T) {
	when := time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC)
	s, err := memory.New(memory.WithClock(func() time.Time { return when }))
	if err != nil {
		t.Fatal(err)
	}
	bundle := storetest.Fixture(t, "clock")
	bundle.Experiment.CreatedAt = when.Add(-24 * time.Hour)
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	if err = s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetExperiment(ctx, bundle.Experiment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.CreatedAt.Equal(when) || !got.UpdatedAt.Equal(when) {
		t.Fatal("caller or wall clock replaced store clock")
	}
	if _, err = memory.New(memory.WithClock(nil)); !errors.Is(err, optimization.ErrInvalidSpec) {
		t.Fatalf("nil clock admitted: %v", err)
	}
}
