package state

import (
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func TestActivationKindPreservesLegacyRoutes(t *testing.T) {
	ctx := operationContext("legacy-activation")
	scope := cortex.ScopeFromContext(ctx)
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	data := newScopeData()
	agent := id.NewAgentID()
	experiment := optimization.Experiment{ID: id.New(optimization.PrefixExperiment), Spec: optimization.ExperimentSpec{Scope: scope, AgentID: agent}}
	decision := optimization.Decision{ID: id.New(optimization.PrefixDecision), ExperimentID: experiment.ID}
	// Old persisted records have no Kind field and use the original head key.
	legacy := optimization.Activation{ID: id.New(optimization.PrefixActivation), Scope: scope, AgentID: agent, Channel: "default", Generation: 1, DecisionID: decision.ID, Actor: optimization.IdentityRef{Subject: "reviewer"}, Reason: "reviewed version"}
	key := agent.String() + "/default"
	data.Experiments[experiment.ID.String()] = experiment
	data.Decisions[decision.ID.String()] = decision
	data.Activations = map[string]optimization.Activation{legacy.ID.String(): legacy}
	data.ActivationHeads = map[string]id.ID{key: legacy.ID}
	s.scopes[scope.Canonical()] = data
	for _, kind := range []optimization.HarnessKind{"", optimization.HarnessExecution} {
		got, readErr := s.GetActivationForKind(ctx, agent, "", kind)
		if readErr != nil || got.ID != legacy.ID {
			t.Fatalf("legacy route: %+v %v", got, readErr)
		}
		change := optimization.ActivationChange{Kind: kind, ID: legacy.ID, AgentID: agent, Channel: "default", Actor: legacy.Actor, DecisionID: decision.ID, Reason: legacy.Reason}
		if retryErr := s.Transact(ctx, func(tx store.Tx) error { _, casErr := tx.CompareAndSwapActivation(change); return casErr }); retryErr != nil {
			t.Fatalf("legacy activation retry: %v", retryErr)
		}
	}
	if _, err = s.GetActivationForKind(ctx, agent, "", optimization.HarnessImprovement); !errors.Is(err, optimization.ErrNotFound) {
		t.Fatalf("legacy route became controller: %v", err)
	}
	if _, err = s.GetActivationForKind(ctx, agent, "", optimization.HarnessKind("unknown")); !errors.Is(err, optimization.ErrInvalidSpec) {
		t.Fatalf("unknown kind accepted: %v", err)
	}
	history, err := s.ListActivationHistory(ctx, agent, "", optimization.PageQuery{})
	if err != nil || len(history.Items) != 1 || history.Items[0].ID != legacy.ID {
		t.Fatalf("legacy history lost: %+v %v", history, err)
	}
}
