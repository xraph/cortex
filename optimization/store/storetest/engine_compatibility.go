package storetest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate" // Register migrations for the shared runtime fixture.

	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	rootsqlite "github.com/xraph/cortex/store/sqlite"
)

func engineCompatibility(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("StaleAgentEditBlocksActiveRunAndRollback", func(t *testing.T) {
		for _, operation := range []string{"execution", "activation"} {
			t.Run(operation, func(t *testing.T) {
				driver := sqlitedriver.New()
				if err := driver.Open(context.Background(), filepath.Join(t.TempDir(), "runtime.db")); err != nil {
					t.Fatal(err)
				}
				db, err := grove.Open(driver)
				if err != nil {
					t.Fatal(err)
				}
				source := rootsqlite.New(db)
				t.Cleanup(func() {
					if closeErr := source.Close(); closeErr != nil {
						t.Error(closeErr)
					}
				})
				if err = source.Migrate(context.Background()); err != nil {
					t.Fatal(err)
				}
				model := &activeModel{}
				runtime, err := engine.New(engine.WithStore(source), engine.WithLLM(model), engine.WithModelRevision("fixture", "1"))
				if err != nil {
					t.Fatal(err)
				}
				var ag *agent.Config
				now := time.Now().UTC()
				data := []byte("source compatibility evidence")
				refs := func(bundle optimization.ExperimentBundle) []optimization.ArtifactRef {
					ref := artifactRegistration(bundle, optimization.WorkLease{}, now).Ref
					ref.Digest, ref.Size = fmt.Sprintf("%x", sha256.Sum256(data)), int64(len(data))
					return []optimization.ArtifactRef{ref}
				}
				s, ctx, bundle, lease, decision := decisionFixtureWithArtifacts(t, factory, nil, refs, func(bundle *optimization.ExperimentBundle) {
					ag = &agent.Config{ID: bundle.Experiment.Spec.AgentID, Name: "production-worker", Model: "fixture", SystemPrompt: "Use current source evidence."}
					sourceCtx := scoped(bundle.Experiment.Spec.Scope)
					if createErr := source.Create(sourceCtx, ag); createErr != nil {
						t.Fatal(createErr)
					}
					prepared, prepareErr := runtime.PrepareAgent(sourceCtx, ag.ID, nil)
					if prepareErr != nil {
						t.Fatal(prepareErr)
					}
					view, inspectErr := prepared.Inspect()
					if inspectErr != nil || len(view.Sections) == 0 {
						t.Fatalf("missing editable prepared sections: %v", inspectErr)
					}
					base := bundle.Versions[0]
					base.Prepared = prepared
					base.Policy.MutableSections = []string{view.Sections[0].ID}
					base, err = optimization.SealVersion(base)
					if err != nil {
						t.Fatal(err)
					}
					bundle.Versions[0] = base
					bundle.Experiment.Spec.Baseline.Digest = base.Digest
					bundle.Experiment.Spec.Objective.MinValidationCases = 1
					bundle.Experiment.Spec.Objective.RequireReproducibility = false
				})
				decision.Outcome, decision.EvidenceClass = optimization.DecisionRecommend, optimization.EvidenceValidated
				decision.Comparison.Complete = true
				decision.Comparison.MissingReasons = nil
				registration := artifactRegistration(bundle, lease, now)
				registration.Ref = refs(bundle)[0]
				registerArtifact(ctx, t, s, registration)
				commitTrialChange(ctx, t, s, decisionChange(ctx, t, s, bundle, lease, decision))
				checker, err := optimization.NewEngineCompatibility(optimization.EngineCompatibilityConfig{Engine: runtime, Host: &activationCompatibility{}, RuntimeVersion: bundle.Versions[0].RuntimeVersion})
				if err != nil {
					t.Fatal(err)
				}
				authority := &publicationAuthority{actor: bundle.Experiment.Spec.Requester}
				mutateSource := false
				reader := activationArtifactReader(func(readCtx context.Context, ref optimization.ArtifactRef) (optimization.Artifact, error) {
					if mutateSource {
						mutateSource = false
						ag.SystemPrompt = "changed during artifact content read"
						if updateErr := source.Update(readCtx, ag); updateErr != nil {
							return optimization.Artifact{}, updateErr
						}
					}
					return optimization.Artifact{Ref: ref, Data: data, CreatedAt: registration.CreatedAt, ExpiresAt: registration.ExpiresAt}, nil
				})
				verifier, err := optimization.NewArtifactVerifier(reader, 1<<20, time.Now)
				if err != nil {
					t.Fatal(err)
				}
				op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Caller: authority, Authorizer: authority, IdentityResolver: authority, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig(), Compatibility: checker, ArtifactVerifier: verifier}, optimization.WithActiveEngine(runtime))
				if err != nil {
					t.Fatal(err)
				}
				active, err := op.Activate(ctx, optimization.ActivationRequest{ID: id.New(optimization.PrefixActivation), AgentID: ag.ID, Version: decision.Candidate, DecisionID: decision.ID, Reason: "reviewed improvement"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = op.RunActive(ctx, optimization.ActiveRunRequest{AgentID: ag.ID, Input: "work"}); err != nil || model.calls != 1 {
					t.Fatalf("qualified active run: calls=%d err=%v", model.calls, err)
				}
				mutateSource = true
				if operation == "activation" {
					if _, err = op.Activate(ctx, optimization.ActivationRequest{ID: id.New(optimization.PrefixActivation), AgentID: ag.ID, Version: decision.Candidate, DecisionID: decision.ID, ExpectedGeneration: 1, Reason: "repeat reviewed activation"}); !errors.Is(err, optimization.ErrStaleVersion) {
						t.Fatalf("source changed during content read remained activatable: %v", err)
					}
				}
				if _, err = op.RunActive(ctx, optimization.ActiveRunRequest{AgentID: ag.ID, Input: "work"}); !errors.Is(err, optimization.ErrStaleVersion) || model.calls != 1 {
					t.Fatalf("stale source reached execution: calls=%d err=%v", model.calls, err)
				}
				if _, err = op.Rollback(ctx, optimization.RollbackRequest{ID: id.New(optimization.PrefixActivation), AgentID: ag.ID, TargetID: active.ID, ExpectedGeneration: 1, Reason: "attempt stale rollback"}); !errors.Is(err, optimization.ErrStaleVersion) {
					t.Fatalf("rollback ignored changed source: %v", err)
				}
				current, err := s.GetActivation(ctx, ag.ID, "")
				if err != nil || current.Generation != 1 {
					t.Fatalf("stale rollback changed history: %+v %v", current, err)
				}
				if _, err = op.Deactivate(ctx, optimization.DeactivationRequest{ID: id.New(optimization.PrefixActivation), AgentID: ag.ID, ExpectedGeneration: 1, Reason: "disable stale route"}); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
}
