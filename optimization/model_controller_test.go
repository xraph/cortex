package optimization_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/optimization"
)

func TestControllerCallsUseActualEstimatorAndLedger(t *testing.T) {
	s, ctx, req, _ := meterFixture(t)
	experiment, err := s.GetExperiment(ctx, req.ExperimentID)
	if err != nil {
		t.Fatal(err)
	}
	base, err := s.GetVersion(ctx, experiment.Spec.Baseline.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"propose", "compose", "reflect"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			provider := modelClient{complete: func(ctx context.Context, r *llm.Request) (*llm.Response, error) {
				calls++
				deadline, bounded := ctx.Deadline()
				if !bounded || time.Until(deadline) > experiment.Spec.Limits.TrialTimeout {
					t.Fatal("controller call has no reserved deadline")
				}
				entries, listErr := s.ListReservations(ctx, experiment.ID, optimization.PageQuery{Limit: 100})
				if listErr != nil {
					t.Fatal(listErr)
				}
				found := false
				for _, entry := range entries.Items {
					if entry.Call != nil && entry.Request.CallKey == "controller/"+mode+"/0" {
						found = true
					}
				}
				if !found {
					t.Fatal("controller model dispatched without ledger marker")
				}
				if r.MaxTokens != 10 || len(r.Messages) != 1 || r.Messages[0].Content == "" {
					t.Fatal("missing bounded controller input")
				}
				content := `{"proposals":[{"edits":[],"rationale":"fixture"}]}`
				if mode == "compose" {
					content = `{"proposal":{"edits":[],"rationale":"fixture"}}`
				}
				if mode == "reflect" {
					content = `{"lessons":[{"statement":"fixture","domain":"test"}]}`
				}
				return &llm.Response{Content: content, Model: "fixture"}, nil
			}}
			factory, createErr := optimization.NewControllerFactory(s, meterAuthorizer(func(context.Context, optimization.Action, optimization.Resource) error { return nil }), meterResolver(func(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
				return ctx, nil
			}), []optimization.ModelBinding{{Model: "fixture", Revision: "1", Provider: provider, Counter: inputBound(func(_ context.Context, r *llm.Request) (optimization.InputTokenBound, error) {
				if !strings.Contains(r.System, "JSON") {
					t.Error("counter did not receive controller instructions")
				}
				return optimization.InputTokenBound{Model: "fixture", ModelRevision: "1", Tokens: 1, Bounded: true}, nil
			})}}, optimization.ControllerLimits{MaxInputBytes: 1 << 20, MaxResponseBytes: 4096, MaxOutputTokens: 10, MaxProposals: 2, MaxLessons: 2})
			if createErr != nil {
				t.Fatal(createErr)
			}
			controller, createErr := factory.ForWork(ctx, optimization.WorkItem{Experiment: experiment, Lease: req.Lease})
			if createErr != nil {
				t.Fatal(createErr)
			}
			input := optimization.ProposalInput{Baseline: base, Policy: base.Policy, Round: experiment.Round}
			invoke := func() error {
				switch mode {
				case "propose":
					_, callErr := controller.Propose(ctx, input)
					return callErr
				case "compose":
					_, callErr := controller.Compose(ctx, optimization.CompositionInput{ProposalInput: input})
					return callErr
				default:
					_, callErr := controller.Reflect(ctx, optimization.ReflectionInput{Domain: "test"})
					return callErr
				}
			}
			if err = invoke(); err != nil {
				t.Fatal(err)
			}
			if err = invoke(); !errors.Is(err, optimization.ErrUnknownAttempt) || calls != 1 {
				t.Fatalf("controller replay: %d %v", calls, err)
			}
		})
	}
}

func TestControllerRefusesHeldOutEvidenceBeforeProvider(t *testing.T) {
	s, ctx, req, _ := meterFixture(t)
	experiment, err := s.GetExperiment(ctx, req.ExperimentID)
	if err != nil {
		t.Fatal(err)
	}
	base, err := s.GetVersion(ctx, experiment.Spec.Baseline.ID)
	if err != nil {
		t.Fatal(err)
	}
	factory, err := optimization.NewControllerFactory(s, meterAuthorizer(func(context.Context, optimization.Action, optimization.Resource) error { return nil }), meterResolver(func(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
		return ctx, nil
	}), []optimization.ModelBinding{{Model: "fixture", Revision: "1", Provider: modelClient{complete: func(context.Context, *llm.Request) (*llm.Response, error) {
		t.Fatal("held-out evidence reached provider")
		return nil, nil
	}}, Counter: inputBound(func(context.Context, *llm.Request) (optimization.InputTokenBound, error) {
		t.Fatal("held-out evidence reached counter")
		return optimization.InputTokenBound{}, nil
	})}}, optimization.ControllerLimits{MaxInputBytes: 1 << 20, MaxResponseBytes: 4096, MaxOutputTokens: 10, MaxProposals: 2, MaxLessons: 2})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := factory.ForWork(ctx, optimization.WorkItem{Experiment: experiment, Lease: req.Lease})
	if err != nil {
		t.Fatal(err)
	}
	evidence := []optimization.DiscoveryEvidence{{Trial: optimization.Trial{Split: optimization.SplitValidation, ExperimentID: experiment.ID}}}
	input := optimization.ProposalInput{Baseline: base, Policy: base.Policy, Evidence: evidence}
	if _, err = controller.Propose(ctx, input); !errors.Is(err, optimization.ErrPolicyDenied) {
		t.Fatal(err)
	}
	if _, err = controller.Compose(ctx, optimization.CompositionInput{ProposalInput: input}); !errors.Is(err, optimization.ErrPolicyDenied) {
		t.Fatal(err)
	}
	if _, err = controller.Reflect(ctx, optimization.ReflectionInput{Evidence: evidence, Domain: "test"}); !errors.Is(err, optimization.ErrPolicyDenied) {
		t.Fatal(err)
	}
	page, err := s.ListReservations(ctx, experiment.ID, optimization.PageQuery{Limit: 10})
	if err != nil || len(page.Items) != 0 {
		t.Fatal("held-out input reserved usage")
	}
}
