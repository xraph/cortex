package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
	rootsqlite "github.com/xraph/cortex/store/sqlite"
)

// teamCaptureStore retains real runtime captures through the shared decision fixture.
type teamCaptureStore struct {
	store.Store
	runtime *engine.Engine
	source  *rootsqlite.Store
}

func (s *teamCaptureStore) captureTrial(ctx context.Context, t *testing.T, bundle optimization.ExperimentBundle, trial optimization.Trial, receipt optimization.EnvironmentReceipt) optimization.ExecutionRecord {
	t.Helper()
	version := bundle.Versions[0]
	view, err := version.Orchestration.Prepared.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	policy := orchestration.BudgetPolicy{SchemaVersion: 1, ExecutionID: id.NewOrchestrationID(), PreparedDigest: version.Orchestration.Prepared.Digest(), Scope: version.Scope, Limits: version.Policy.OrchestrationPresets[0].Limits, Deadline: time.Now().Add(time.Minute), TokenOnly: true, Participants: map[string]execution.Resources{}, ParticipantDigests: map[string]string{}}
	for name, p := range view.Participants {
		child, inspectErr := p.Inspect()
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		b := child.Config.Budget
		policy.Participants[name] = execution.Resources{Steps: int64(b.MaxSteps), Tokens: int64(b.MaxTokens), ToolCalls: int64(b.MaxToolCalls), FanOut: 1}
		policy.ParticipantDigests[name] = p.Digest()
	}
	var input string
	for _, task := range bundle.Experiment.Spec.Tasks {
		if task.ID == trial.CaseID {
			input = task.Input
		}
	}
	result, err := s.runtime.RunPreparedOrchestration(ctx, version.Orchestration.Prepared, input, engine.PreparedRunOptions{Purpose: run.PurposeOptimizationTrial, OrchestrationBudget: &policy, Provenance: run.ExecutionProvenance{ExperimentID: bundle.Experiment.ID.String(), TrialID: trial.ID.String(), CandidateID: trial.CandidateID.String(), EnvironmentReceiptID: receipt.ID, HarnessID: version.HarnessID.String(), HarnessVersion: version.ID.String()}})
	if err != nil {
		t.Fatal(err)
	}
	capture, err := optimization.CaptureOrchestration(ctx, s.source, version, receipt, result.ID)
	if err != nil {
		t.Fatal(err)
	}
	return capture
}

type activeTeamAuthority struct {
	*publicationAuthority
	experimentDenied bool
}

func (a *activeTeamAuthority) Authorize(ctx context.Context, action optimization.Action, resource optimization.Resource) error {
	if a.experimentDenied && resource.Kind == "experiment" {
		return optimization.ErrPolicyDenied
	}
	return a.publicationAuthority.Authorize(ctx, action, resource)
}

type activeTeamHost struct{ before func() }

func (h *activeTeamHost) Check(_ context.Context, version optimization.HarnessVersion) (optimization.CompatibilityReport, error) {
	if h.before != nil {
		h.before()
	}
	return optimization.CompatibilityReport{Version: optimization.VersionRef{ID: version.ID, Digest: version.Digest}, PolicyDigest: version.PolicyDigest, Compatible: true}, nil
}

func activeTeamExecution(t *testing.T, factory Factory) {
	t.Helper()
	activeTeamLifecycle(t, factory, false)
}

// ActiveTeamRecovery checks reviewed execution after both stores are reopened.
func ActiveTeamRecovery(t *testing.T, open func(*testing.T, func() time.Time) store.Store) {
	t.Helper()
	activeTeamLifecycle(t, open, true)
}

func activeTeamLifecycle(t *testing.T, factory Factory, recoverStores bool) {
	t.Helper()
	t.Run("RunActiveTeamUsesReviewedEvidenceAndCurrentGrants", func(t *testing.T) {
		model := &teamTrialModel{}
		bundle, runtime, source, reopen := OrchestrationFixture(t, model)
		wrapped := func(t *testing.T, now func() time.Time) store.Store {
			return &teamCaptureStore{Store: factory(t, now), runtime: runtime, source: source}
		}
		s, ctx, bundle, lease, decision := decisionFixture(t, wrapped, func(b *optimization.ExperimentBundle) {
			*b = bundle
			b.Experiment.Spec.Limits.Repetitions = 1
			b.Experiment.Spec.Objective.MinValidationCases = 1
			b.Experiment.Spec.Objective.RequireReproducibility = false
		})
		decision.Outcome, decision.EvidenceClass, decision.Comparison.Complete, decision.Comparison.MissingReasons = optimization.DecisionRecommend, optimization.EvidenceValidated, true, nil
		commitTrialChange(ctx, t, s, decisionChange(ctx, t, s, bundle, lease, decision))
		change := activationRequest(bundle, decision)
		authority := &activeTeamAuthority{publicationAuthority: &publicationAuthority{actor: change.Actor}}
		host := &activeTeamHost{}
		makeOptimizer := func(repository store.Store, boundRuntime *engine.Engine) *optimization.Optimizer {
			t.Helper()
			checker, err := optimization.NewEngineCompatibility(optimization.EngineCompatibilityConfig{Engine: boundRuntime, RuntimeVersion: bundle.Versions[0].RuntimeVersion, Host: host})
			if err != nil {
				t.Fatal(err)
			}
			verifier, err := optimization.NewArtifactVerifier(unavailableActivationArtifacts{}, 1<<20, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: repository, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig(), Compatibility: checker, ArtifactVerifier: verifier}, optimization.WithActiveEngine(boundRuntime))
			if err != nil {
				t.Fatal(err)
			}
			return op
		}
		op := makeOptimizer(s, runtime)
		active, err := op.Activate(ctx, optimization.ActivationRequest{ID: change.ID, AgentID: change.AgentID, Version: change.Version, DecisionID: change.DecisionID, Reason: "reviewed team"})
		if err != nil {
			t.Fatal(err)
		}
		if recoverStores {
			if closeErr := s.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			s = factory(t, time.Now)
			freshRuntime, createErr := engine.New(engine.WithStore(reopen()), engine.WithLLM(model), engine.WithModelRevision("fixture", "1"))
			if createErr != nil {
				t.Fatal(createErr)
			}
			op = makeOptimizer(s, freshRuntime)
		}
		before := model.calls.Load()
		request := optimization.ActiveOrchestrationRequest{AgentID: change.AgentID, Input: "production task"}
		if _, err = op.RunActive(ctx, optimization.ActiveRunRequest{AgentID: change.AgentID}); !errors.Is(err, optimization.ErrUnavailable) {
			t.Fatalf("agent route accepted team: %v", err)
		}
		authority.denied = true
		if _, err = op.RunActiveOrchestration(ctx, request); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("denied team dispatched: %v", err)
		}
		authority.denied = false
		result, err := op.RunActiveOrchestration(ctx, request)
		if err != nil || result == nil || result.Status != orchestration.StatusCompleted || len(result.AgentRunIDs) != 2 {
			t.Fatalf("active team: %+v %v", result, err)
		}
		if model.calls.Load() != before+2 {
			t.Fatal("unexpected production dispatch count")
		}
		reopened := reopen()
		budget, err := reopened.GetOrchestrationBudgetSnapshot(ctx, result.ID)
		if err != nil {
			t.Fatal(err)
		}
		version, err := s.GetVersion(ctx, active.Version.ID)
		if err != nil {
			t.Fatal(err)
		}
		if budget.Policy.Limits != version.Policy.OrchestrationPresets[0].Limits || budget.Policy.PreparedDigest != version.Orchestration.Prepared.Digest() || !budget.Policy.TokenOnly {
			t.Fatal("approved shared policy changed")
		}
		sessions := map[id.ID]bool{}
		for _, key := range result.AgentRunIDs {
			child, readErr := reopened.GetRun(ctx, key)
			if readErr != nil {
				t.Fatal(readErr)
			}
			p := child.Provenance
			if p.Purpose != run.PurposeNormal || p.HarnessVersion != version.ID.String() || p.ExperimentID != bundle.Experiment.ID.String() || p.TrialID != "" || p.EnvironmentReceiptID != "" || child.SessionID.IsNil() || sessions[child.SessionID] {
				t.Fatalf("production provenance/session: %+v", child)
			}
			sessions[child.SessionID] = true
		}
		// A grant withdrawn during an external compatibility check cannot dispatch.
		host.before = func() { authority.denied = true }
		if _, err = op.RunActiveOrchestration(ctx, request); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("revoked grant dispatched: %v", err)
		}
		authority.denied = false
		host.before = nil
		host.before = func() { authority.experimentDenied = true }
		if _, err = op.RunActiveOrchestration(ctx, request); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("revoked experiment grant dispatched: %v", err)
		}
		authority.experimentDenied = false
		host.before = nil
		// The invocation pins its version at entry; deactivation blocks the next invocation.
		host.before = func() {
			_, disableErr := op.Deactivate(ctx, optimization.DeactivationRequest{ID: id.New(optimization.PrefixActivation), AgentID: change.AgentID, ExpectedGeneration: 1, Reason: "disable during compatibility"})
			if disableErr != nil {
				t.Fatal(disableErr)
			}
		}
		pinned, err := op.RunActiveOrchestration(ctx, request)
		if err != nil || pinned == nil || pinned.Status != orchestration.StatusCompleted {
			t.Fatalf("pinned team changed: %+v %v", pinned, err)
		}
		host.before = nil
		if _, err = op.RunActiveOrchestration(ctx, request); !errors.Is(err, optimization.ErrInactiveActivation) {
			t.Fatalf("disabled route dispatched: %v", err)
		}
		rolled, err := op.Rollback(ctx, optimization.RollbackRequest{ID: id.New(optimization.PrefixActivation), AgentID: change.AgentID, TargetID: active.ID, ExpectedGeneration: 2, Reason: "restore reviewed team"})
		if err != nil || rolled.Generation != 3 {
			t.Fatalf("team rollback: %+v %v", rolled, err)
		}
		reviewer, err := source.GetByName(ctx, "reviewer")
		if err != nil {
			t.Fatal(err)
		}
		reviewer.SystemPrompt = "changed after review"
		if err = source.Update(ctx, reviewer); err != nil {
			t.Fatal(err)
		}
		if _, err = op.RunActiveOrchestration(ctx, request); !errors.Is(err, optimization.ErrStaleVersion) {
			t.Fatalf("changed secondary participant dispatched: %v", err)
		}
		if model.calls.Load() != before+4 {
			t.Fatal("refused team reached provider")
		}
	})
}
