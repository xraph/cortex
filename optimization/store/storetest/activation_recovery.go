package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/store/scopespy"
)

// ActivationRecovery executes a stored activation after reopening its database,
// then verifies that a disabled generation and its idempotency survive another reopen.
func ActivationRecovery(t *testing.T, open func(*testing.T, func() time.Time) store.Store) {
	t.Helper()
	first, ctx, change := activationFixture(t, open)
	active := activate(ctx, t, first, change)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := open(t, time.Now)
	authority := &publicationAuthority{actor: change.Actor}
	verifier, err := optimization.NewArtifactVerifier(unavailableActivationArtifacts{}, 1<<20, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	model := &activeModel{}
	runtime, err := engine.New(engine.WithStore(scopespy.New()), engine.WithLLM(model), engine.WithModelRevision("fixture", "1"))
	if err != nil {
		t.Fatal(err)
	}
	config := optimization.OptimizerConfig{Repository: second, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig(), Compatibility: &activationCompatibility{}, ArtifactVerifier: verifier}
	op, err := optimization.NewOptimizer(config, optimization.WithActiveEngine(runtime))
	if err != nil {
		t.Fatal(err)
	}
	result, err := op.RunActive(ctx, optimization.ActiveRunRequest{AgentID: change.AgentID, Input: "after restart"})
	if err != nil || result.Provenance.HarnessVersion != active.Version.ID.String() {
		t.Fatalf("reopened active execution: %+v %v", result, err)
	}
	request := optimization.DeactivationRequest{ID: id.New(optimization.PrefixActivation), AgentID: active.AgentID, ExpectedGeneration: 1, Reason: "disable across restart"}
	disabled, err := op.Deactivate(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = second.Close(); err != nil {
		t.Fatal(err)
	}
	third := open(t, time.Now)
	config.Repository = third
	op, err = optimization.NewOptimizer(config, optimization.WithActiveEngine(runtime))
	if err != nil {
		t.Fatal(err)
	}
	retry, err := op.Deactivate(ctx, request)
	if err != nil || retry.ID != disabled.ID || retry.Generation != 2 {
		t.Fatalf("reopened deactivation retry: %+v %v", retry, err)
	}
	if _, err = op.RunActive(ctx, optimization.ActiveRunRequest{AgentID: active.AgentID, Input: "refused"}); !errors.Is(err, optimization.ErrInactiveActivation) {
		t.Fatalf("reopened disabled route: %v", err)
	}
	if model.calls != 1 {
		t.Fatal("reopened disabled route reached model")
	}
	decision, err := third.GetDecision(ctx, change.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := third.ListEvents(ctx, decision.ExperimentID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events.Items {
		if event.Type == "activation_deactivated" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("reopened audit count: %d", count)
	}
}
