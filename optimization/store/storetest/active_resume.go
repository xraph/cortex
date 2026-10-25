package storetest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/scopespy"
)

type activePauseModel struct{ calls int }

func (m *activePauseModel) Complete(_ context.Context, request *llm.Request) (*llm.Response, error) {
	m.calls++
	if m.calls == 1 {
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "pending", Name: "external", Arguments: `{}`}}, FinishReason: "tool_calls", Model: request.Model, Usage: llm.Usage{TotalTokens: 1}}, nil
	}
	return &llm.Response{Content: request.System, FinishReason: "stop", Model: request.Model, Usage: llm.Usage{TotalTokens: 1}}, nil
}
func (*activePauseModel) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, errors.New("unexpected stream")
}

func activeResume(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("RunActiveResumeRetainsSnapshotAfterDeactivation", func(t *testing.T) {
		model := &activePauseModel{}
		runtime, err := engine.New(engine.WithStore(scopespy.New()), engine.WithLLM(model), engine.WithModelRevision("fixture", "1"), engine.WithExternalTool(llm.Tool{Name: "external"}), engine.WithToolRevision("external", "1"))
		if err != nil {
			t.Fatal(err)
		}
		s, ctx, bundle, lease, decision := decisionFixture(t, factory, func(bundle *optimization.ExperimentBundle) {
			bundle.Experiment.Spec.Objective.MinValidationCases = 1
			bundle.Experiment.Spec.Objective.RequireReproducibility = false
			prepared, prepareErr := runtime.PrepareAgent(scoped(bundle.Experiment.Spec.Scope), bundle.Experiment.Spec.AgentID, &engine.RunOverrides{SystemPrompt: "original", Model: "fixture", Tools: []string{"external"}, ToolsRestricted: true, MaxSteps: 3})
			if prepareErr != nil {
				t.Fatal(prepareErr)
			}
			base := bundle.Versions[0]
			base.Prepared = prepared
			sealed, sealErr := optimization.SealVersion(base)
			if sealErr != nil {
				t.Fatal(sealErr)
			}
			bundle.Versions[0] = sealed
			bundle.Experiment.Spec.Baseline = optimization.VersionRef{ID: sealed.ID, Digest: sealed.Digest}
		})
		decision.Outcome, decision.EvidenceClass, decision.Comparison.Complete, decision.Comparison.MissingReasons = optimization.DecisionRecommend, optimization.EvidenceValidated, true, nil
		commitTrialChange(ctx, t, s, decisionChange(ctx, t, s, bundle, lease, decision))
		change := activationRequest(bundle, decision)
		activate(ctx, t, s, change)
		authority := &publicationAuthority{actor: change.Actor}
		verifier, err := optimization.NewArtifactVerifier(unavailableActivationArtifacts{}, 1<<20, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig(), Compatibility: &activationCompatibility{}, ArtifactVerifier: verifier}, optimization.WithActiveEngine(runtime))
		if err != nil {
			t.Fatal(err)
		}
		paused, err := op.RunActive(ctx, optimization.ActiveRunRequest{AgentID: change.AgentID, Input: "work"})
		if err != nil {
			t.Fatal(err)
		}
		if paused.State != run.StatePaused {
			t.Fatalf("did not suspend: %s", paused.State)
		}
		if _, err = op.Deactivate(ctx, optimization.DeactivationRequest{ID: id.New(optimization.PrefixActivation), AgentID: change.AgentID, ExpectedGeneration: 1, Reason: "disable new work"}); err != nil {
			t.Fatal(err)
		}
		resumed, err := runtime.Resume(ctx, paused.ID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "pending", Content: "result"}}})
		if err != nil {
			t.Fatal(err)
		}
		if resumed.State != run.StateCompleted || resumed.Provenance.PreparedDigest != paused.Provenance.PreparedDigest || resumed.Provenance.HarnessVersion != change.Version.ID.String() || !strings.HasPrefix(resumed.Output, "Check the evidence before answering.") {
			t.Fatalf("resume lost selected snapshot: %+v", resumed)
		}
	})
}
