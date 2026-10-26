package storetest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func retainCommands(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("RetainBaselineController", func(t *testing.T) { RetainControllerBaseline(t, factory) })
	t.Run("RetainBaselinePreservesDecisionAndFencesCommands", func(t *testing.T) {
		s, ctx, change := ReviewedActivationFixture(t, factory)
		decision, err := s.GetDecision(ctx, change.DecisionID)
		if err != nil {
			t.Fatal(err)
		}
		current, err := s.GetExperiment(ctx, decision.ExperimentID)
		if err != nil {
			t.Fatal(err)
		}
		authority := &publicationAuthority{actor: optimization.IdentityRef{Subject: "reviewer"}}
		service := commandOptimizer(t, s, authority)
		request := optimization.RetainRequest{CommandRequest: optimization.CommandRequest{ExperimentID: current.ID, ExpectedRevision: current.Revision, IdempotencyKey: "retain-review"}, RetainDecision: optimization.RetainDecision{DecisionID: decision.ID, Reason: "Keep the baseline for this release"}}
		for _, reason := range []string{"", "   "} {
			invalid := request
			invalid.Reason = reason
			if _, err = service.RetainCommand(ctx, invalid); !errors.Is(err, optimization.ErrInvalidSpec) {
				t.Fatalf("invalid reason: %v", err)
			}
		}
		invalid := request
		invalid.DecisionID = id.New(optimization.PrefixDecision)
		if _, err = service.RetainCommand(ctx, invalid); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("wrong decision: %v", err)
		}
		invalid = request
		invalid.ExpectedRevision--
		if _, err = service.RetainCommand(ctx, invalid); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("stale revision: %v", err)
		}
		authority.denied = true
		if _, err = service.RetainCommand(ctx, request); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("denied review: %v", err)
		}
		authority.denied = false
		retained, err := service.RetainCommand(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if retained.State != optimization.StateCompleted || retained.Revision != current.Revision+1 || retained.RetainedBaseline == nil {
			t.Fatalf("review: %+v", retained)
		}
		review := retained.RetainedBaseline
		if review.DecisionID != decision.ID || review.Baseline != decision.Baseline || review.Actor != authority.actor || review.Reason != request.Reason || review.CreatedAt.IsZero() {
			t.Fatalf("review identity: %+v", review)
		}
		measured, err := s.GetDecision(ctx, decision.ID)
		if err != nil || !reflect.DeepEqual(measured, decision) {
			t.Fatalf("review changed measured evidence: %+v %v", measured, err)
		}
		comparison, err := service.GetComparison(ctx, current.ID, optimization.PageQuery{})
		if err != nil || comparison.RetainedBaseline == nil || comparison.RetainedBaseline.Reason != request.Reason || !reflect.DeepEqual(*comparison.Decision, decision) {
			t.Fatalf("comparison lost review or measurement: %+v %v", comparison, err)
		}
		repeat, err := service.RetainCommand(ctx, request)
		if err != nil || repeat.Revision != retained.Revision {
			t.Fatalf("repeat: %+v %v", repeat, err)
		}
		changed := request
		changed.Reason = "different decision"
		if _, err = service.RetainCommand(ctx, changed); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("changed content: %v", err)
		}
		authority.actor.Subject = "other-reviewer"
		if _, err = service.RetainCommand(ctx, request); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("changed reviewer: %v", err)
		}
		authority.actor.Subject = "reviewer"
		authority.denied = true
		if _, err = service.RetainCommand(ctx, request); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("retry skipped current grant: %v", err)
		}
		authority.denied = false
		if _, err = service.CancelCommand(ctx, request.CommandRequest); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("changed action: %v", err)
		}
		changed = request
		changed.IdempotencyKey = "another-review"
		changed.ExpectedRevision = retained.Revision
		if _, err = service.RetainCommand(ctx, changed); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("second review: %v", err)
		}
		if err = s.Transact(ctx, func(tx store.Tx) error { _, e := tx.CompareAndSwapActivation(change); return e }); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("retained candidate activated: %v", err)
		}
		if _, err = s.GetActivation(ctx, change.AgentID, ""); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("retention changed production route: %v", err)
		}
		events, err := s.ListEvents(ctx, current.ID, 0, 500)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, event := range events.Items {
			if event.Type == "baseline_retained" {
				count++
				if event.Actor != authority.actor {
					t.Fatal("review audit actor lost")
				}
			}
		}
		if count != 1 {
			t.Fatalf("review event count: %d", count)
		}
		review.Reason = "caller mutation"
		saved, err := s.GetExperiment(ctx, current.ID)
		if err != nil || saved.RetainedBaseline.Reason != request.Reason {
			t.Fatal("review aliases storage")
		}
	})
	t.Run("RetainBaselineCannotContradictActiveRoute", func(t *testing.T) {
		s, ctx, change := ReviewedActivationFixture(t, factory)
		decision, err := s.GetDecision(ctx, change.DecisionID)
		if err != nil {
			t.Fatal(err)
		}
		current, err := s.GetExperiment(ctx, decision.ExperimentID)
		if err != nil {
			t.Fatal(err)
		}
		active := activate(ctx, t, s, change)
		authority := &publicationAuthority{actor: change.Actor}
		service := commandOptimizer(t, s, authority)
		request := optimization.RetainRequest{CommandRequest: optimization.CommandRequest{ExperimentID: current.ID, ExpectedRevision: current.Revision, IdempotencyKey: "retain"}, RetainDecision: optimization.RetainDecision{DecisionID: decision.ID, Reason: "retain baseline"}}
		if _, err = service.RetainCommand(ctx, request); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("active recommendation retained: %v", err)
		}
		head, err := s.GetActivation(ctx, change.AgentID, "")
		if err != nil || head.ID != active.ID {
			t.Fatal("failed review changed route")
		}
		if _, err = service.Deactivate(ctx, optimization.DeactivationRequest{ID: id.New(optimization.PrefixActivation), AgentID: change.AgentID, ExpectedGeneration: active.Generation, Reason: "disable reviewed route"}); err != nil {
			t.Fatal(err)
		}
		retained, err := service.RetainCommand(ctx, request)
		if err != nil || retained.RetainedBaseline == nil {
			t.Fatalf("retain after explicit deactivation: %+v %v", retained, err)
		}
	})
	t.Run("RetainBaselineAndActivationCommitExclusively", func(t *testing.T) {
		s, ctx, change := ReviewedActivationFixture(t, factory)
		decision, err := s.GetDecision(ctx, change.DecisionID)
		if err != nil {
			t.Fatal(err)
		}
		current, err := s.GetExperiment(ctx, decision.ExperimentID)
		if err != nil {
			t.Fatal(err)
		}
		service := commandOptimizer(t, s, &publicationAuthority{actor: change.Actor})
		request := optimization.RetainRequest{CommandRequest: optimization.CommandRequest{ExperimentID: current.ID, ExpectedRevision: current.Revision, IdempotencyKey: "concurrent-retain"}, RetainDecision: optimization.RetainDecision{DecisionID: decision.ID, Reason: "keep baseline"}}
		outcomes := make(chan error, 2)
		start := make(chan struct{})
		go func() { <-start; _, e := service.RetainCommand(ctx, request); outcomes <- e }()
		go func() {
			<-start
			outcomes <- s.Transact(ctx, func(tx store.Tx) error { _, e := tx.CompareAndSwapActivation(change); return e })
		}()
		close(start)
		successes := 0
		for range 2 {
			e := <-outcomes
			if e == nil {
				successes++
			} else if !errors.Is(e, optimization.ErrConflict) && !errors.Is(e, optimization.ErrPolicyDenied) {
				t.Fatal(e)
			}
		}
		if successes != 1 {
			t.Fatalf("conflicting reviews committed: %d", successes)
		}
		saved, err := s.GetExperiment(ctx, current.ID)
		if err != nil {
			t.Fatal(err)
		}
		active, activeErr := s.GetActivation(ctx, change.AgentID, "")
		if saved.RetainedBaseline != nil {
			if !errors.Is(activeErr, optimization.ErrNotFound) {
				t.Fatalf("retained review has active route: %+v %v", active, activeErr)
			}
		} else if activeErr != nil || active.DecisionID != decision.ID {
			t.Fatalf("winning activation missing: %+v %v", active, activeErr)
		}
	})
	t.Run("RetainBaselineRejectsForgedWorkerPayloads", func(t *testing.T) {
		s, ctx, change := ReviewedActivationFixture(t, factory)
		decision, err := s.GetDecision(ctx, change.DecisionID)
		if err != nil {
			t.Fatal(err)
		}
		current, err := s.GetExperiment(ctx, decision.ExperimentID)
		if err != nil {
			t.Fatal(err)
		}
		base := optimization.Transition{CommandAction: optimization.ActionRetain, ExperimentID: current.ID, ExpectedRevision: current.Revision, NextState: optimization.StateCompleted, IdempotencyKey: "forged", Actor: change.Actor, Retain: &optimization.RetainDecision{DecisionID: decision.ID, Reason: "keep baseline"}}
		for _, mutate := range []func(*optimization.Transition){
			func(c *optimization.Transition) { c.CommandAction = "" },
			func(c *optimization.Transition) { c.CommandAction = optimization.ActionCancel },
			func(c *optimization.Transition) { c.Decision = &decision },
			func(c *optimization.Transition) { c.NextPhase = optimization.PhaseReflect },
			func(c *optimization.Transition) { c.Retain = nil },
		} {
			forged := base
			mutate(&forged)
			if err = s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(forged) }); !errors.Is(err, optimization.ErrInvalidSpec) {
				t.Fatalf("forged review: %v", err)
			}
		}
		bundle := Fixture(t, "forged-retention")
		bundle.Experiment.RetainedBaseline = &optimization.RetainedBaseline{DecisionID: decision.ID}
		if err = s.Transact(scoped(bundle.Experiment.Spec.Scope), func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatalf("forged initial review: %v", err)
		}
	})
}

// RetainRecovery verifies that the review and its retry identity survive reopening.
func RetainRecovery(t *testing.T, open func(*testing.T, func() time.Time) store.Store) {
	t.Helper()
	s, ctx, change := ReviewedActivationFixture(t, open)
	decision, err := s.GetDecision(ctx, change.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.GetExperiment(ctx, decision.ExperimentID)
	if err != nil {
		t.Fatal(err)
	}
	authority := &publicationAuthority{actor: change.Actor}
	service := commandOptimizer(t, s, authority)
	request := optimization.RetainRequest{CommandRequest: optimization.CommandRequest{ExperimentID: current.ID, ExpectedRevision: current.Revision, IdempotencyKey: "lost-review-response"}, RetainDecision: optimization.RetainDecision{DecisionID: decision.ID, Reason: "retain after review"}}
	retained, err := service.RetainCommand(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, time.Now)
	service = commandOptimizer(t, s, authority)
	recovered, err := service.RetainCommand(ctx, request)
	if err != nil || !reflect.DeepEqual(retained, recovered) {
		t.Fatalf("recovered review: %+v %v", recovered, err)
	}
	if err = s.Transact(ctx, func(tx store.Tx) error { _, e := tx.CompareAndSwapActivation(change); return e }); !errors.Is(err, optimization.ErrPolicyDenied) {
		t.Fatalf("reopened retained candidate activated: %v", err)
	}
	authority.denied = true
	if _, err = service.RetainCommand(ctx, request); !errors.Is(err, optimization.ErrPolicyDenied) {
		t.Fatalf("reopened current grant: %v", err)
	}
}

// finishReviewFixture completes fixture reflection without creating new evidence.
func finishReviewFixture(ctx context.Context, t *testing.T, s store.Store, experimentID id.ID) {
	t.Helper()
	current, err := s.GetExperiment(ctx, experimentID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Transact(ctx, func(tx store.Tx) error {
		return tx.CommitTransition(optimization.Transition{ExperimentID: current.ID, ExpectedRevision: current.Revision, NextState: optimization.StateReviewRequired, Lease: current.Lease, Actor: current.Spec.Requester, IdempotencyKey: "fixture-review"})
	}); err != nil {
		t.Fatal(err)
	}
}

// RetainControllerBaseline reviews the parent without retaining its execution baseline.
func RetainControllerBaseline(t *testing.T, factory Factory) {
	t.Helper()
	f, _, _, decision := controllerDecisionFixture(t, factory, controllerDecisionOptions{qualified: true, improved: true})
	commitTrialChange(f.ctx, t, f.s, decisionChange(f.ctx, t, f.s, f.parent, f.leases[0], decision))
	finishControllerReview(t, f)
	current, err := f.s.GetExperiment(f.ctx, f.parent.Experiment.ID)
	if err != nil {
		t.Fatal(err)
	}
	authority := &publicationAuthority{actor: optimization.IdentityRef{Subject: "reviewer"}}
	service := commandOptimizer(t, f.s, authority)
	request := optimization.RetainRequest{CommandRequest: optimization.CommandRequest{ExperimentID: current.ID, ExpectedRevision: current.Revision, IdempotencyKey: "retain-controller"}, RetainDecision: optimization.RetainDecision{DecisionID: decision.ID, Reason: "keep current controller"}}
	retained, err := service.RetainCommand(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if retained.RetainedBaseline == nil || retained.RetainedBaseline.Baseline != f.parent.Experiment.Spec.Improvement.Controllers[0] || retained.RetainedBaseline.Baseline == f.parent.Experiment.Spec.Baseline {
		t.Fatalf("wrong retained controller: %+v", retained.RetainedBaseline)
	}
	change := optimization.ActivationChange{ID: id.New(optimization.PrefixActivation), Kind: optimization.HarnessImprovement, AgentID: current.Spec.AgentID, Version: decision.Candidate, DecisionID: decision.ID, Actor: authority.actor, PolicyDigest: decision.PolicyDigest}
	if err = f.s.Transact(f.ctx, func(tx store.Tx) error { _, e := tx.CompareAndSwapActivation(change); return e }); !errors.Is(err, optimization.ErrPolicyDenied) {
		t.Fatalf("retained controller activated: %v", err)
	}
}
