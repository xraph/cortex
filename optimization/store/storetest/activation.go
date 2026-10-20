package storetest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func activations(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("ActivationCASHistoryAndAudit", func(t *testing.T) {
		s, ctx, request := activationFixture(t, factory)
		first := activate(ctx, t, s, request)
		if first.Generation != 1 || first.ExpectedGeneration != 0 || first.Channel != "default" || first.CreatedAt.IsZero() || first.Actor.Subject != request.Actor.Subject {
			t.Fatalf("invalid first generation: %+v", first)
		}
		secondRequest := request
		secondRequest.ID = id.New(optimization.PrefixActivation)
		secondRequest.ExpectedGeneration = 1
		second := activate(ctx, t, s, secondRequest)
		if second.Generation != 2 || second.ID == first.ID {
			t.Fatal("activation history overwritten")
		}
		// Retrying an older successful command returns its record without moving the head.
		retry := activate(ctx, t, s, request)
		current, err := s.GetActivation(ctx, request.AgentID, "")
		if err != nil || current.ID != second.ID || retry.ID != first.ID || retry.Generation != 1 {
			t.Fatalf("retry moved activation head: %+v %+v %v", retry, current, err)
		}
		history, err := s.GetActivationGeneration(ctx, first.ID)
		if err != nil || history.Generation != 1 {
			t.Fatal("previous generation lost")
		}
		current.Scope.Levels[0].Value = "changed"
		again, err := s.GetActivation(ctx, request.AgentID, "default")
		if err != nil || again.Scope.Levels[0].Value == "changed" {
			t.Fatal("activation read aliases storage")
		}
		decision, err := s.GetDecision(ctx, request.DecisionID)
		if err != nil {
			t.Fatal(err)
		}
		events, err := s.ListEvents(ctx, decision.ExperimentID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		var activationEvents int
		for _, event := range events.Items {
			if event.Type == "activated" {
				activationEvents++
				if event.Actor.Subject != request.Actor.Subject || !slices.Contains(event.Resources, request.DecisionID) || !slices.Contains(event.Resources, request.Version.ID) {
					t.Fatal("activation audit lost actor or evidence")
				}
			}
		}
		if activationEvents != 2 {
			t.Fatalf("duplicate or missing activation events: %d", activationEvents)
		}
		foreign := scoped(Fixture(t, "foreign-activation").Experiment.Spec.Scope)
		if _, err = s.GetActivationGeneration(foreign, first.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("history leaked: %v", err)
		}
		if _, err = s.GetActivation(foreign, request.AgentID, "default"); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("head leaked: %v", err)
		}
		if _, err = s.GetActivation(context.Background(), request.AgentID, "default"); err == nil {
			t.Fatal("unscoped activation read")
		}
	})
	t.Run("ActivationRejectsInvalidEvidenceAndConflictingRetry", func(t *testing.T) {
		s, ctx, request := activationFixture(t, factory)
		for _, mode := range []string{"agent", "version", "digest", "decision", "policy", "actor", "channel", "negative", "generation", "id", "rollback"} {
			t.Run(mode, func(t *testing.T) {
				changed := request
				switch mode {
				case "agent":
					changed.AgentID = id.NewAgentID()
				case "version":
					changed.Version.ID = id.New(optimization.PrefixVersion)
				case "digest":
					changed.Version.Digest = strings.Repeat("f", 64)
				case "decision":
					changed.DecisionID = id.New(optimization.PrefixDecision)
				case "policy":
					changed.PolicyDigest = strings.Repeat("f", 64)
				case "actor":
					changed.Actor.Subject = " "
				case "channel":
					changed.Channel = " default "
				case "negative":
					changed.ExpectedGeneration = -1
				case "generation":
					changed.ExpectedGeneration = 1
				case "id":
					changed.ID = id.New(optimization.PrefixTrial)
				case "rollback":
					changed.RollbackTo = id.New(optimization.PrefixActivation)
				}
				abort := errors.New("invalid activation accepted")
				err := s.Transact(ctx, func(tx store.Tx) error {
					if _, err := tx.CompareAndSwapActivation(changed); err != nil {
						return err
					}
					return abort
				})
				if errors.Is(err, abort) {
					t.Fatal(mode)
				}
			})
		}
		activate(ctx, t, s, request)
		request.Actor.Subject = "different reviewer"
		if err := s.Transact(ctx, func(tx store.Tx) error { _, err := tx.CompareAndSwapActivation(request); return err }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("conflicting command reused ID: %v", err)
		}
	})
	t.Run("ActivationRejectsInconclusiveDecision", func(t *testing.T) {
		s, ctx, bundle, lease, decision := decisionFixture(t, factory)
		commitTrialChange(ctx, t, s, decisionChange(ctx, t, s, bundle, lease, decision))
		request := activationRequest(bundle, decision)
		if err := s.Transact(ctx, func(tx store.Tx) error { _, err := tx.CompareAndSwapActivation(request); return err }); !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("inconclusive activated: %v", err)
		}
	})
	t.Run("ActivationConcurrentReviewersConflict", func(t *testing.T) {
		s, ctx, request := activationFixture(t, factory)
		var wg sync.WaitGroup
		results := make(chan error, 2)
		for range 2 {
			changed := request
			changed.ID = id.New(optimization.PrefixActivation)
			wg.Go(func() {
				results <- s.Transact(ctx, func(tx store.Tx) error { _, err := tx.CompareAndSwapActivation(changed); return err })
			})
		}
		wg.Wait()
		close(results)
		wins, conflicts := 0, 0
		for err := range results {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, optimization.ErrConflict):
				conflicts++
			default:
				t.Fatal(err)
			}
		}
		if wins != 1 || conflicts != 1 {
			t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
		}
	})
	t.Run("ActivationRollbackAtomicAndAppendOnly", func(t *testing.T) {
		s, ctx, request := activationFixture(t, factory)
		first := activate(ctx, t, s, request)
		request.ID = id.New(optimization.PrefixActivation)
		request.ExpectedGeneration = 1
		request.RollbackTo = first.ID
		abort := errors.New("abort activation")
		if err := s.Transact(ctx, func(tx store.Tx) error {
			if _, err := tx.CompareAndSwapActivation(request); err != nil {
				return err
			}
			return abort
		}); !errors.Is(err, abort) {
			t.Fatal(err)
		}
		if _, err := s.GetActivationGeneration(ctx, request.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatal("aborted generation remains")
		}
		current, err := s.GetActivation(ctx, request.AgentID, request.Channel)
		if err != nil || current.ID != first.ID {
			t.Fatal("aborted activation changed head")
		}
		rolled := activate(ctx, t, s, request)
		if rolled.Generation != 2 || rolled.RollbackTo != first.ID || rolled.Version != first.Version || rolled.DecisionID != first.DecisionID {
			t.Fatalf("rollback rewound history: %+v", rolled)
		}
		crossChannel := request
		crossChannel.ID = id.New(optimization.PrefixActivation)
		crossChannel.Channel = "preview"
		crossChannel.ExpectedGeneration = 0
		if err = s.Transact(ctx, func(tx store.Tx) error {
			_, activationErr := tx.CompareAndSwapActivation(crossChannel)
			return activationErr
		}); err == nil {
			t.Fatal("rollback crossed channel")
		}
		crossChannel.RollbackTo = id.Nil
		preview := activate(ctx, t, s, crossChannel)
		if preview.Generation != 1 {
			t.Fatal("channels share generations")
		}
	})
}

func activationFixture(t *testing.T, factory Factory) (store.Store, context.Context, optimization.ActivationChange) {
	t.Helper()
	s, ctx, bundle, lease, decision := decisionFixture(t, factory, func(bundle *optimization.ExperimentBundle) {
		bundle.Experiment.Spec.Objective.MinValidationCases = 1
		bundle.Experiment.Spec.Objective.RequireReproducibility = false
	})
	decision.Outcome = optimization.DecisionRecommend
	decision.EvidenceClass = optimization.EvidenceValidated
	decision.Comparison.Complete = true
	decision.Comparison.MissingReasons = nil
	commitTrialChange(ctx, t, s, decisionChange(ctx, t, s, bundle, lease, decision))
	return s, ctx, activationRequest(bundle, decision)
}

func activationRequest(bundle optimization.ExperimentBundle, decision optimization.Decision) optimization.ActivationChange {
	return optimization.ActivationChange{ID: id.New(optimization.PrefixActivation), AgentID: bundle.Experiment.Spec.AgentID, Version: decision.Candidate, DecisionID: decision.ID, PolicyDigest: decision.PolicyDigest, Actor: optimization.IdentityRef{Subject: "reviewer"}}
}

func activate(ctx context.Context, t *testing.T, s store.Store, request optimization.ActivationChange) optimization.Activation {
	t.Helper()
	var value optimization.Activation
	if err := s.Transact(ctx, func(tx store.Tx) error { var err error; value, err = tx.CompareAndSwapActivation(request); return err }); err != nil {
		t.Fatal(err)
	}
	return value
}
