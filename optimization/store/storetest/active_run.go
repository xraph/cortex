package storetest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/scopespy"
)

type activeModel struct {
	before func()
	calls  int
}

func (m *activeModel) Complete(_ context.Context, request *llm.Request) (*llm.Response, error) {
	m.calls++
	if m.before != nil {
		m.before()
	}
	return &llm.Response{Content: request.System, FinishReason: "stop", Model: request.Model, Usage: llm.Usage{TotalTokens: 1}}, nil
}
func (*activeModel) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, errors.New("unexpected stream")
}

func activeExecution(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("RunActiveUsesBoundEngineAndPinsVersionAtStart", func(t *testing.T) {
		s, ctx, change := activationFixture(t, factory)
		activate(ctx, t, s, change)
		authority := &publicationAuthority{actor: change.Actor}
		verifier, err := optimization.NewArtifactVerifier(unavailableActivationArtifacts{}, 1<<20, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		compatibility := &activationCompatibility{}
		config := optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig(), Compatibility: compatibility, ArtifactVerifier: verifier}
		unbound, err := optimization.NewOptimizer(config)
		if err != nil {
			t.Fatal(err)
		}
		request := optimization.ActiveRunRequest{AgentID: change.AgentID, Input: "production work"}
		if _, err = unbound.RunActive(ctx, request); !errors.Is(err, optimization.ErrUnavailable) {
			t.Fatalf("unbound execution: %v", err)
		}
		model := &activeModel{}
		runtime, err := engine.New(engine.WithStore(scopespy.New()), engine.WithLLM(model), engine.WithModelRevision("fixture", "1"))
		if err != nil {
			t.Fatal(err)
		}
		op, err := optimization.NewOptimizer(config, optimization.WithActiveEngine(runtime))
		if err != nil {
			t.Fatal(err)
		}
		authority.denied = true
		if _, err = op.RunActive(ctx, request); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("unauthorized active run: %v", err)
		}
		authority.denied = false
		compatibility.incompatible = true
		if _, err = op.RunActive(ctx, request); !errors.Is(err, optimization.ErrStaleVersion) {
			t.Fatalf("stale source ran: %v", err)
		}
		compatibility.incompatible = false
		if err = engine.WithModelRevision("fixture", "2")(runtime); err != nil {
			t.Fatal(err)
		}
		if _, err = op.RunActive(ctx, request); !errors.Is(err, optimization.ErrStaleVersion) {
			t.Fatalf("production model pin ignored: %v", err)
		}
		if model.calls != 0 {
			t.Fatal("denied or stale run reached model")
		}
		if err = engine.WithModelRevision("fixture", "1")(runtime); err != nil {
			t.Fatal(err)
		}
		model.before = func() {
			if _, disableErr := op.Deactivate(ctx, optimization.DeactivationRequest{ID: id.New(optimization.PrefixActivation), AgentID: change.AgentID, ExpectedGeneration: 1, Reason: "disable while running"}); disableErr != nil {
				t.Fatal(disableErr)
			}
		}
		result, err := op.RunActive(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		version, err := s.GetVersion(ctx, change.Version.ID)
		if err != nil {
			t.Fatal(err)
		}
		if model.calls != 1 || result.State != run.StateCompleted || result.Provenance.PreparedDigest != version.Prepared.Digest() || result.Provenance.HarnessVersion != version.ID.String() || result.Provenance.Purpose != run.PurposeNormal {
			t.Fatalf("unbound active run: %+v", result)
		}
		if _, err = op.RunActive(ctx, request); !errors.Is(err, optimization.ErrInactiveActivation) {
			t.Fatalf("disabled route executed: %v", err)
		}
		if model.calls != 1 {
			t.Fatal("disabled route reached model")
		}
		model.before = nil
		ordinary, err := runtime.RunAgent(ctx, "ordinary", "work", &engine.RunOverrides{SystemPrompt: "ordinary prompt", Model: "fixture", ToolsRestricted: true})
		if err != nil || !strings.HasPrefix(ordinary.Output, "ordinary prompt\n") || ordinary.Provenance.HarnessVersion != "" {
			t.Fatalf("ordinary route changed: %+v %v", ordinary, err)
		}
	})
}

type activationArtifactReader func(context.Context, optimization.ArtifactRef) (optimization.Artifact, error)

func (f activationArtifactReader) Get(ctx context.Context, ref optimization.ArtifactRef) (optimization.Artifact, error) {
	return f(ctx, ref)
}

func activeEvidenceRace(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("RunActiveRechecksEvidenceAfterExternalRead", func(t *testing.T) {
		now := time.Now().UTC()
		data := []byte("retained evidence")
		digest := fmt.Sprintf("%x", sha256.Sum256(data))
		refs := func(bundle optimization.ExperimentBundle) []optimization.ArtifactRef {
			ref := artifactRegistration(bundle, optimization.WorkLease{}, now).Ref
			ref.Digest = digest
			ref.Size = int64(len(data))
			return []optimization.ArtifactRef{ref}
		}
		s, ctx, bundle, lease, decision := decisionFixtureWithArtifacts(t, factory, nil, refs, func(bundle *optimization.ExperimentBundle) {
			bundle.Experiment.Spec.Objective.MinValidationCases = 1
			bundle.Experiment.Spec.Objective.RequireReproducibility = false
		})
		registration := artifactRegistration(bundle, lease, now)
		registration.Ref = refs(bundle)[0]
		registerArtifact(ctx, t, s, registration)
		decision.Outcome, decision.EvidenceClass, decision.Comparison.Complete, decision.Comparison.MissingReasons = optimization.DecisionRecommend, optimization.EvidenceValidated, true, nil
		commitTrialChange(ctx, t, s, decisionChange(ctx, t, s, bundle, lease, decision))
		change := activationRequest(bundle, decision)
		activate(ctx, t, s, change)
		authority := &publicationAuthority{actor: change.Actor}
		reader := activationArtifactReader(func(_ context.Context, ref optimization.ArtifactRef) (optimization.Artifact, error) {
			err := s.Transact(ctx, func(tx store.Tx) error {
				return tx.BeginArtifactDeletion(optimization.ArtifactDeletion{Ref: ref, RequestID: "withdraw-during-active-read", Actor: change.Actor, Reason: "source withdrawn"})
			})
			if err != nil {
				return optimization.Artifact{}, err
			}
			return optimization.Artifact{Ref: ref, Data: data, CreatedAt: registration.CreatedAt, ExpiresAt: registration.ExpiresAt}, nil
		})
		verifier, err := optimization.NewArtifactVerifier(reader, 1<<20, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		model := &activeModel{}
		runtime, err := engine.New(engine.WithStore(scopespy.New()), engine.WithLLM(model), engine.WithModelRevision("fixture", "1"))
		if err != nil {
			t.Fatal(err)
		}
		op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig(), Compatibility: &activationCompatibility{}, ArtifactVerifier: verifier}, optimization.WithActiveEngine(runtime))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = op.RunActive(ctx, optimization.ActiveRunRequest{AgentID: change.AgentID, Input: "work"}); !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("withdrawn evidence dispatched: %v", err)
		}
		if model.calls != 0 {
			t.Fatal("withdrawn evidence reached model")
		}
	})
}
