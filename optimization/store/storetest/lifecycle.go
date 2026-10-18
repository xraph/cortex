package storetest

import (
	"errors"
	"testing"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func lifecycle(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("DraftEventAtomic", func(t *testing.T) {
		s := factory(t)
		bundle := Fixture(t, "draft-event")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		abort := errors.New("abort")
		if err := s.Transact(ctx, func(tx store.Tx) error {
			if err := tx.CreateExperiment(bundle); err != nil {
				return err
			}
			return abort
		}); !errors.Is(err, abort) {
			t.Fatal(err)
		}
		if _, err := s.ListEvents(ctx, bundle.Experiment.ID, 0, 10); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("orphan draft event: %v", err)
		}
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
			t.Fatal(err)
		}
		events, err := s.ListEvents(ctx, bundle.Experiment.ID, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(events.Items) != 1 || events.Items[0].Sequence != 1 || events.Items[0].Type != "draft" || events.Items[0].Actor.Subject != bundle.Experiment.Spec.Requester.Subject {
			t.Fatalf("missing creation audit: %+v", events)
		}
	})
	t.Run("DuplicateStartIdempotency", func(t *testing.T) {
		s := factory(t)
		bundle := Fixture(t, "start")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
			t.Fatal(err)
		}
		start := optimization.Transition{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 1, NextState: optimization.StateQueued, IdempotencyKey: "start", Actor: bundle.Experiment.Spec.Requester}
		apply := func(change optimization.Transition) error {
			return s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) })
		}
		if err := apply(start); err != nil {
			t.Fatal(err)
		}
		if err := apply(start); err != nil {
			t.Fatalf("matching retry failed: %v", err)
		}
		got, err := s.GetExperiment(ctx, bundle.Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != optimization.StateQueued || got.Revision != 2 {
			t.Fatalf("duplicate start advanced revision: %+v", got)
		}
		events, err := s.ListEvents(ctx, got.ID, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(events.Items) != 2 || events.Items[1].Sequence != 2 {
			t.Fatalf("duplicate audit or lost order: %+v", events)
		}
		start.NextState = optimization.StateCancelled
		if err = apply(start); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("idempotency key changed payload: %v", err)
		}
		start.IdempotencyKey = "cancel"
		if err = apply(start); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("stale revision accepted: %v", err)
		}
		start.ExpectedRevision = 2
		if err = apply(start); err != nil {
			t.Fatal(err)
		}
		start.ExpectedRevision = 3
		start.IdempotencyKey = "restart"
		start.NextState = optimization.StateQueued
		if err = apply(start); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("terminal experiment restarted: %v", err)
		}
	})
	t.Run("TransitionRollback", func(t *testing.T) {
		s := factory(t)
		bundle := Fixture(t, "transition-rollback")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
			t.Fatal(err)
		}
		abort := errors.New("abort")
		err := s.Transact(ctx, func(tx store.Tx) error {
			if commitErr := tx.CommitTransition(optimization.Transition{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 1, NextState: optimization.StateQueued, IdempotencyKey: "start", Actor: bundle.Experiment.Spec.Requester}); commitErr != nil {
				return commitErr
			}
			return abort
		})
		if !errors.Is(err, abort) {
			t.Fatal(err)
		}
		got, err := s.GetExperiment(ctx, bundle.Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		events, err := s.ListEvents(ctx, bundle.Experiment.ID, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != optimization.StateDraft || got.Revision != 1 || len(events.Items) != 1 {
			t.Fatal("state or audit survived rollback")
		}
	})
	t.Run("CursorPaging", func(t *testing.T) {
		s := factory(t)
		bundle := Fixture(t, "paging")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		ids := map[id.ID]bool{}
		for range 3 {
			bundle.Experiment.ID = id.New(optimization.PrefixExperiment)
			ids[bundle.Experiment.ID] = true
			if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
				t.Fatal(err)
			}
		}
		cursor := ""
		for range 3 {
			page, err := s.ListExperiments(ctx, optimization.ListQuery{Limit: 1, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 || !ids[page.Items[0].ID] {
				t.Fatalf("duplicate or foreign identity: %+v", page)
			}
			delete(ids, page.Items[0].ID)
			cursor = page.NextCursor
		}
		if len(ids) != 0 || cursor != "" {
			t.Fatalf("incomplete cursor traversal: %v %s", ids, cursor)
		}
		if _, err := s.ListExperiments(ctx, optimization.ListQuery{Limit: -1}); err == nil {
			t.Fatal("negative limit accepted")
		}
	})
}
