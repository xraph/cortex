package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/operator"
	"github.com/xraph/cortex/optimization/store"
)

// ReviewedActivationFixture saves a complete recommendation for transport tests.
// Its deterministic evidence is not a live-provider qualification.
func ReviewedActivationFixture(t *testing.T, factory Factory) (store.Store, context.Context, optimization.ActivationChange) {
	t.Helper()
	s, ctx, change := activationFixture(t, factory)
	decision, err := s.GetDecision(ctx, change.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	finishReviewFixture(ctx, t, s, decision.ExperimentID)
	return s, ctx, change
}

func activationOperator(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("OperatorActivationCommandsBindEnvelopeAndCurrentAuthority", func(t *testing.T) {
		repo, ctx, change := activationFixture(t, factory)
		config, authority, compatibility := activationOperatorConfig(t, repo)
		service, err := optimization.NewOptimizer(config)
		if err != nil {
			t.Fatal(err)
		}
		commands := operator.Commands{Optimizer: service}
		input := operator.ActivateInput{ActivationRouteInput: operator.ActivationRouteInput{AgentID: change.AgentID.String(), Reason: "reviewed improvement"}, VersionID: change.Version.ID.String(), VersionDigest: change.Version.Digest, DecisionID: change.DecisionID.String()}
		for _, key := range []string{"", " padded", "bad\nkey"} {
			if _, err = commands.Activate(ctx, input, key); !errors.Is(err, optimization.ErrInvalidSpec) {
				t.Fatalf("invalid key: %v", err)
			}
		}
		if _, err = commands.Activate(context.Background(), input, "activate"); !errors.Is(err, cortex.ErrNoScope) {
			t.Fatalf("missing scope: %v", err)
		}
		authority.denied = true
		if _, err = commands.Activate(ctx, input, "activate"); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("denied: %v", err)
		}
		authority.denied = false
		results := make(chan *optimization.Activation, 8)
		failures := make(chan error, 8)
		var concurrent sync.WaitGroup
		for range 8 {
			concurrent.Go(func() {
				value, callErr := commands.Activate(ctx, input, "activate")
				results <- value
				failures <- callErr
			})
		}
		concurrent.Wait()
		close(results)
		close(failures)
		conflicts := 0
		for callErr := range failures {
			if errors.Is(callErr, optimization.ErrConflict) {
				conflicts++
			} else if callErr != nil {
				t.Fatal(callErr)
			}
		}
		var first *optimization.Activation
		for value := range results {
			if value == nil {
				continue
			}
			if first != nil && (value.ID != first.ID || value.Generation != first.Generation) {
				t.Fatal("concurrent command repeated activation")
			}
			first = value
		}
		if first == nil {
			t.Fatal("no concurrent command committed")
		}
		// A backend may exhaust its bounded fence retries under contention.
		// Retrying those calls after contention must find the same durable receipt.
		for range conflicts {
			recovered, retryErr := commands.Activate(ctx, input, "activate")
			if retryErr != nil || recovered.ID != first.ID || recovered.Generation != first.Generation {
				t.Fatalf("conflict recovery repeated command: %+v %v", recovered, retryErr)
			}
		}
		_, err = commands.Activate(ctx, input, "activate")
		if err != nil {
			t.Fatal(err)
		}
		if first.ID.Prefix() != optimization.PrefixActivation || first.Generation != 1 {
			t.Fatalf("activation: %+v", first)
		}
		service, err = optimization.NewOptimizer(config)
		if err != nil {
			t.Fatal(err)
		}
		commands = operator.Commands{Optimizer: service}
		retry, err := commands.Activate(ctx, input, "activate")
		if err != nil || retry.ID != first.ID || retry.Generation != 1 {
			t.Fatalf("fresh service retry: %+v %v", retry, err)
		}
		altered := input
		altered.Reason = "changed review"
		if _, err = commands.Activate(ctx, altered, "activate"); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("changed reason: %v", err)
		}
		altered = input
		altered.ExpectedGeneration = 1
		if _, err = commands.Activate(ctx, altered, "activate"); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("changed generation: %v", err)
		}
		altered = input
		altered.Channel = "preview"
		if _, err = commands.Activate(ctx, altered, "activate"); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("changed route: %v", err)
		}
		if _, err = commands.Activate(ctx, input, "fresh-key-stale-generation"); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("fresh key stale generation: %v", err)
		}
		authority.actor.Subject = "different-reviewer"
		if _, err = commands.Activate(ctx, input, "activate"); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("changed actor: %v", err)
		}
		authority.actor.Subject = "reviewer"
		authority.denied = true
		if _, err = commands.Activate(ctx, input, "activate"); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("cached grant: %v", err)
		}
		authority.denied = false
		compatibility.incompatible = true
		if _, err = commands.Activate(ctx, input, "activate"); !errors.Is(err, optimization.ErrStaleVersion) {
			t.Fatalf("cached compatibility: %v", err)
		}
		compatibility.incompatible = false
		rollback := operator.RollbackInput{ActivationRouteInput: input.ActivationRouteInput, TargetID: first.ID.String()}
		rollback.ExpectedGeneration = 1
		if _, err = commands.Rollback(ctx, rollback, "activate"); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("activation key changed action: %v", err)
		}
		rolled, err := commands.Rollback(ctx, rollback, "rollback")
		if err != nil || rolled.Generation != 2 || rolled.RollbackTo != first.ID {
			t.Fatalf("rollback: %+v %v", rolled, err)
		}
		disable := input.ActivationRouteInput
		disable.ExpectedGeneration = 2
		if _, err = commands.Deactivate(ctx, disable, "rollback"); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("rollback key changed action: %v", err)
		}
		disabled, err := commands.Deactivate(ctx, disable, "deactivate")
		if err != nil || !disabled.Deactivated || disabled.Generation != 3 {
			t.Fatalf("disable: %+v %v", disabled, err)
		}
		for _, invoke := range []func() (*optimization.Activation, error){func() (*optimization.Activation, error) { return commands.Activate(ctx, input, "activate") }, func() (*optimization.Activation, error) { return commands.Rollback(ctx, rollback, "rollback") }, func() (*optimization.Activation, error) { return commands.Deactivate(ctx, disable, "deactivate") }} {
			if _, err = invoke(); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = repo.GetActivation(ctx, change.AgentID, ""); !errors.Is(err, optimization.ErrInactiveActivation) {
			t.Fatalf("old retry changed current route: %v", err)
		}
		input.ExpectedGeneration = 3
		restored, err := commands.Activate(ctx, input, "restore")
		if err != nil || restored.Generation != 4 {
			t.Fatalf("reactivation: %+v %v", restored, err)
		}
		if _, err = commands.Deactivate(ctx, disable, "deactivate"); err != nil {
			t.Fatal(err)
		}
		current, err := repo.GetActivation(ctx, change.AgentID, "")
		if err != nil || current.ID != restored.ID {
			t.Fatalf("old disable changed current route: %+v %v", current, err)
		}
		history, err := repo.ListActivationHistory(ctx, change.AgentID, "", optimization.PageQuery{Limit: 10})
		if err != nil || len(history.Items) != 4 || history.NextCursor != "" {
			t.Fatalf("retries appended generations: %+v %v", history, err)
		}
		foreign := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "foreign"}}})
		if _, err = commands.Activate(foreign, input, "restore"); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("foreign scope: %v", err)
		}
		malformed := input
		malformed.VersionID = id.New(optimization.PrefixExperiment).String()
		if _, err = commands.Activate(ctx, malformed, "bad-version"); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatalf("wrong version kind: %v", err)
		}
		if _, err = (operator.Commands{}).Activate(ctx, input, "unconfigured"); !errors.Is(err, optimization.ErrUnavailable) {
			t.Fatalf("missing optimizer: %v", err)
		}
	})
}

func activationOperatorConfig(t *testing.T, repo store.Store) (optimization.OptimizerConfig, *publicationAuthority, *activationCompatibility) {
	t.Helper()
	authority := &publicationAuthority{actor: optimization.IdentityRef{Subject: "reviewer"}}
	compatibility := &activationCompatibility{}
	verifier, err := optimization.NewArtifactVerifier(unavailableActivationArtifacts{}, 1<<20, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	config := optimization.OptimizerConfig{Repository: repo, Authorizer: authority, Caller: authority, IdentityResolver: authority, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig(), Compatibility: compatibility, ArtifactVerifier: verifier}
	return config, authority, compatibility
}

// OperatorActivationRecovery retries transport commands after reopening each
// committed generation. Old receipts cannot overwrite the current route.
func OperatorActivationRecovery(t *testing.T, open func(*testing.T, func() time.Time) store.Store) {
	t.Helper()
	repo, ctx, change := activationFixture(t, open)
	config, authority, _ := activationOperatorConfig(t, repo)
	newCommands := func() operator.Commands {
		config.Repository = repo
		service, err := optimization.NewOptimizer(config)
		if err != nil {
			t.Fatal(err)
		}
		return operator.Commands{Optimizer: service}
	}
	commands := newCommands()
	reopen := func() {
		if err := repo.Close(); err != nil {
			t.Fatal(err)
		}
		repo = open(t, time.Now)
		commands = newCommands()
	}
	input := operator.ActivateInput{ActivationRouteInput: operator.ActivationRouteInput{AgentID: change.AgentID.String(), Reason: "reviewed across restart"}, VersionID: change.Version.ID.String(), VersionDigest: change.Version.Digest, DecisionID: change.DecisionID.String()}
	active, err := commands.Activate(ctx, input, "activate-response-lost")
	if err != nil {
		t.Fatal(err)
	}
	reopen()
	retry, err := commands.Activate(ctx, input, "activate-response-lost")
	if err != nil || retry.ID != active.ID || retry.Generation != 1 {
		t.Fatalf("reopened activation: %+v %v", retry, err)
	}
	rollback := operator.RollbackInput{ActivationRouteInput: input.ActivationRouteInput, TargetID: active.ID.String()}
	rollback.ExpectedGeneration = 1
	rolled, err := commands.Rollback(ctx, rollback, "rollback-response-lost")
	if err != nil {
		t.Fatal(err)
	}
	reopen()
	retry, err = commands.Rollback(ctx, rollback, "rollback-response-lost")
	if err != nil || retry.ID != rolled.ID || retry.Generation != 2 {
		t.Fatalf("reopened rollback: %+v %v", retry, err)
	}
	disable := input.ActivationRouteInput
	disable.ExpectedGeneration = 2
	disabled, err := commands.Deactivate(ctx, disable, "deactivate-response-lost")
	if err != nil {
		t.Fatal(err)
	}
	reopen()
	retry, err = commands.Deactivate(ctx, disable, "deactivate-response-lost")
	if err != nil || retry.ID != disabled.ID || retry.Generation != 3 || !retry.Deactivated {
		t.Fatalf("reopened disable: %+v %v", retry, err)
	}
	authority.denied = true
	if _, err = commands.Activate(ctx, input, "activate-response-lost"); !errors.Is(err, optimization.ErrPolicyDenied) {
		t.Fatalf("reopened grant: %v", err)
	}
	authority.denied = false
	if _, err = commands.Activate(ctx, input, "activate-response-lost"); err != nil {
		t.Fatal(err)
	}
	if _, err = commands.Rollback(ctx, rollback, "rollback-response-lost"); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.GetActivation(ctx, change.AgentID, ""); !errors.Is(err, optimization.ErrInactiveActivation) {
		t.Fatalf("old command enabled current route: %v", err)
	}
	decision, err := repo.GetDecision(ctx, change.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := repo.ListEvents(ctx, decision.ExperimentID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"activated", "activation_rolled_back", "activation_deactivated"} {
		count := 0
		for _, event := range events.Items {
			if event.Type == kind {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("%s events after recovery: %d", kind, count)
		}
	}
}
