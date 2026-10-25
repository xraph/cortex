package storetest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

type activationCompatibility struct {
	incompatible bool
	policy       string
}

func (c *activationCompatibility) Check(_ context.Context, version optimization.HarnessVersion) (optimization.CompatibilityReport, error) {
	policy := version.PolicyDigest
	if c.policy != "" {
		policy = c.policy
	}
	return optimization.CompatibilityReport{Version: optimization.VersionRef{ID: version.ID, Digest: version.Digest}, PolicyDigest: policy, Compatible: !c.incompatible}, nil
}

type unavailableActivationArtifacts struct{}

func (unavailableActivationArtifacts) Get(context.Context, optimization.ArtifactRef) (optimization.Artifact, error) {
	return optimization.Artifact{}, optimization.ErrUnavailable
}

func activationCommands(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("ActivationCommandsRequireCurrentAuthorityAndCompatibility", func(t *testing.T) {
		s, ctx, change := activationFixture(t, factory)
		authority := &publicationAuthority{actor: optimization.IdentityRef{Subject: "current-reviewer"}}
		compatibility := &activationCompatibility{}
		verifier, err := optimization.NewArtifactVerifier(unavailableActivationArtifacts{}, 1<<20, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig(), Compatibility: compatibility, ArtifactVerifier: verifier})
		if err != nil {
			t.Fatal(err)
		}
		request := optimization.ActivationRequest{ID: change.ID, AgentID: change.AgentID, Version: change.Version, DecisionID: change.DecisionID, Reason: "validated improvement"}
		authority.denied = true
		if _, err = op.Activate(ctx, request); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("unauthorized activation: %v", err)
		}
		authority.denied = false
		compatibility.incompatible = true
		if _, err = op.Activate(ctx, request); !errors.Is(err, optimization.ErrStaleVersion) {
			t.Fatalf("stale activation: %v", err)
		}
		compatibility.incompatible = false
		active, err := op.Activate(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if active.Generation != 1 || active.Actor != authority.actor {
			t.Fatalf("unattributed activation: %+v", active)
		}
		again, err := op.Activate(ctx, request)
		if err != nil || again.ID != active.ID || again.Generation != 1 {
			t.Fatalf("activation retry: %+v %v", again, err)
		}
		rollback := optimization.RollbackRequest{ID: id.New(optimization.PrefixActivation), AgentID: change.AgentID, TargetID: active.ID, ExpectedGeneration: 1, Reason: "restore reviewed version"}
		compatibility.policy = "changed-policy"
		if _, err = op.Rollback(ctx, rollback); !errors.Is(err, optimization.ErrStaleVersion) {
			t.Fatalf("rollback ignored current policy: %v", err)
		}
		compatibility.policy = ""
		rolled, err := op.Rollback(ctx, rollback)
		if err != nil || rolled.Generation != 2 || rolled.RollbackTo != active.ID {
			t.Fatalf("rollback: %+v %v", rolled, err)
		}
		disabled, err := op.Deactivate(ctx, optimization.DeactivationRequest{ID: id.New(optimization.PrefixActivation), AgentID: change.AgentID, ExpectedGeneration: 2, Reason: "disable route"})
		if err != nil || disabled.Generation != 3 {
			t.Fatalf("deactivation: %+v %v", disabled, err)
		}
		if _, err = s.GetActivation(ctx, change.AgentID, ""); err == nil {
			t.Fatal("disabled route still resolves")
		}
		history, err := s.GetActivationGeneration(ctx, active.ID)
		if err != nil || history.ID != active.ID {
			t.Fatalf("lost history: %v", err)
		}
	})
}

func activationDeactivation(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("DeactivationPersistsHistoryAndFencesRetries", func(t *testing.T) {
		s, ctx, change := activationFixture(t, factory)
		first := activate(ctx, t, s, change)
		request := optimization.DeactivationChange{DeactivationRequest: optimization.DeactivationRequest{ID: id.New(optimization.PrefixActivation), AgentID: first.AgentID, ExpectedGeneration: 1, Reason: "stop routing"}, Actor: change.Actor}
		abort := errors.New("abort deactivation")
		if err := s.Transact(ctx, func(tx store.Tx) error {
			if _, err := tx.Deactivate(request); err != nil {
				return err
			}
			return abort
		}); !errors.Is(err, abort) {
			t.Fatal(err)
		}
		if active, err := s.GetActivation(ctx, first.AgentID, ""); err != nil || active.ID != first.ID {
			t.Fatal("aborted deactivation changed head")
		}
		var disabled optimization.Activation
		for range 2 {
			if err := s.Transact(ctx, func(tx store.Tx) error { var err error; disabled, err = tx.Deactivate(request); return err }); err != nil {
				t.Fatal(err)
			}
		}
		if !disabled.Deactivated || disabled.Generation != 2 || disabled.Reason != request.Reason {
			t.Fatalf("invalid deactivation: %+v", disabled)
		}
		if _, err := s.GetActivation(ctx, first.AgentID, ""); !errors.Is(err, optimization.ErrInactiveActivation) {
			t.Fatalf("disabled head: %v", err)
		}
		decision, err := s.GetDecision(ctx, change.DecisionID)
		if err != nil {
			t.Fatal(err)
		}
		events, err := s.ListEvents(ctx, decision.ExperimentID, 0, 500)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, event := range events.Items {
			if event.Type == "activation_deactivated" {
				count++
				if event.Actor != request.Actor || !strings.Contains(string(event.Payload), "stop routing") {
					t.Fatal("deactivation audit missing")
				}
			}
		}
		if count != 1 {
			t.Fatalf("deactivation event count: %d", count)
		}
		change.ID = id.New(optimization.PrefixActivation)
		change.ExpectedGeneration = 2
		restored := activate(ctx, t, s, change)
		if restored.Generation != 3 || restored.Deactivated {
			t.Fatal("reactivation lost generation")
		}
		if err = s.Transact(ctx, func(tx store.Tx) error { _, retryErr := tx.Deactivate(request); return retryErr }); err != nil {
			t.Fatal(err)
		}
		active, err := s.GetActivation(ctx, first.AgentID, "")
		if err != nil || active.ID != restored.ID {
			t.Fatal("old retry disabled newer route")
		}
		request.Reason = "conflicting retry"
		if err = s.Transact(ctx, func(tx store.Tx) error { _, retryErr := tx.Deactivate(request); return retryErr }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("retry conflict: %v", err)
		}
		change.ID = id.New(optimization.PrefixActivation)
		change.ExpectedGeneration = 3
		change.RollbackTo = disabled.ID
		if err = s.Transact(ctx, func(tx store.Tx) error { _, retryErr := tx.CompareAndSwapActivation(change); return retryErr }); !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("disabled generation used for rollback: %v", err)
		}
	})
	t.Run("ActivationRequiresFreshArtifactContentButDeactivationDoesNot", func(t *testing.T) {
		now := time.Now().UTC()
		fixed := func(t *testing.T, _ func() time.Time) store.Store { return factory(t, func() time.Time { return now }) }
		refs := func(bundle optimization.ExperimentBundle) []optimization.ArtifactRef {
			return []optimization.ArtifactRef{artifactRegistration(bundle, optimization.WorkLease{}, now).Ref}
		}
		s, ctx, bundle, lease, decision := decisionFixtureWithArtifacts(t, fixed, nil, refs, func(bundle *optimization.ExperimentBundle) {
			bundle.Experiment.Spec.Objective.MinValidationCases = 1
			bundle.Experiment.Spec.Objective.RequireReproducibility = false
		})
		registration := artifactRegistration(bundle, lease, now)
		registerArtifact(ctx, t, s, registration)
		decision.Outcome, decision.EvidenceClass, decision.Comparison.Complete, decision.Comparison.MissingReasons = optimization.DecisionRecommend, optimization.EvidenceValidated, true, nil
		commitTrialChange(ctx, t, s, decisionChange(ctx, t, s, bundle, lease, decision))
		authority := &publicationAuthority{actor: optimization.IdentityRef{Subject: "reviewer"}}
		verifier, err := optimization.NewArtifactVerifier(unavailableActivationArtifacts{}, 1<<20, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		config := optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig(), Compatibility: &activationCompatibility{}, ArtifactVerifier: verifier}
		op, err := optimization.NewOptimizer(config)
		if err != nil {
			t.Fatal(err)
		}
		change := activationRequest(bundle, decision)
		req := optimization.ActivationRequest{ID: change.ID, AgentID: change.AgentID, Version: change.Version, DecisionID: change.DecisionID, Reason: "review"}
		if _, err = op.Activate(ctx, req); !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("metadata alone allowed activation: %v", err)
		}
		if _, err = s.GetActivation(ctx, change.AgentID, ""); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatal("failed evidence check persisted activation")
		}
		active := activate(ctx, t, s, change)
		now = registration.ExpiresAt
		// Disabling must not require the original requester's continuing access or old evidence.
		config.Compatibility = nil
		config.ArtifactVerifier = nil
		op, err = optimization.NewOptimizer(config)
		if err != nil {
			t.Fatal(err)
		}
		disabled, err := op.Deactivate(ctx, optimization.DeactivationRequest{ID: id.New(optimization.PrefixActivation), AgentID: change.AgentID, ExpectedGeneration: active.Generation, Reason: "evidence expired"})
		if err != nil || !disabled.Deactivated {
			t.Fatalf("expired route could not be disabled: %+v %v", disabled, err)
		}
	})
}
