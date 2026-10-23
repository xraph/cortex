package storetest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

type controllerProvider struct {
	body  string
	calls int
}

func (p *controllerProvider) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	p.calls++
	return &llm.Response{Content: p.body, Model: "fixture"}, nil
}
func (*controllerProvider) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, optimization.ErrUnavailable
}

type controllerCounter struct{}

func (controllerCounter) Bound(context.Context, *llm.Request) (optimization.InputTokenBound, error) {
	return optimization.InputTokenBound{Model: "fixture", ModelRevision: "1", Tokens: 100, Bounded: true}, nil
}

type controllerAuthority struct{ actor optimization.IdentityRef }

func (controllerAuthority) Authorize(context.Context, optimization.Action, optimization.Resource) error {
	return nil
}
func (controllerAuthority) Resolve(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
	return ctx, nil
}
func (a controllerAuthority) Current(context.Context) (optimization.IdentityRef, error) {
	return a.actor, nil
}

func controllerWorker(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("WorkerCommitsMeteredControllerCandidate", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := candidateBundle(t)
		ctx := scoped(bundle.Experiment.Spec.Scope)
		proposalExperiment(ctx, t, s, bundle)
		candidate, _ := candidateVersion(t, bundle, "Check the available evidence.")
		body, err := json.Marshal(map[string]any{"proposals": []optimization.Proposal{candidate.Proposal}})
		if err != nil {
			t.Fatal(err)
		}
		provider := &controllerProvider{body: string(body)}
		authority := controllerAuthority{actor: bundle.Experiment.Spec.Requester}
		models, err := optimization.NewControllerFactory(s, authority, authority, []optimization.ModelBinding{{Model: "fixture", Revision: "1", Provider: provider, Counter: controllerCounter{}}}, optimization.ControllerLimits{MaxInputBytes: 1 << 20, MaxResponseBytes: 4096, MaxOutputTokens: 10, MaxProposals: 2, MaxLessons: 2})
		if err != nil {
			t.Fatal(err)
		}
		handler, err := optimization.NewControllerPhaseHandler(s, models, nil, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: handler, Worker: optimization.DefaultWorkerConfig()})
		if err != nil {
			t.Fatal(err)
		}
		current, err := s.GetExperiment(ctx, bundle.Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		paused, err := op.Pause(ctx, current.ID, current.Revision)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = op.Resume(ctx, current.ID, paused.Revision); err != nil {
			t.Fatal(err)
		}
		if err = op.WorkOnce(ctx, current.ID); err != nil {
			t.Fatal(err)
		}
		final, err := s.GetExperiment(ctx, current.ID)
		if err != nil || final.Phase != optimization.PhaseProbe || provider.calls != 1 {
			t.Fatalf("worker result: %+v %v calls=%d", final, err, provider.calls)
		}
		page, err := s.ListCandidates(ctx, current.ID, optimization.PageQuery{Limit: 10})
		if err != nil || len(page.Items) != 1 || !page.Items[0].Accepted {
			t.Fatalf("candidate not committed: %+v %v", page, err)
		}
		usage, err := s.GetReservation(ctx, current.ID, "controller/propose/1")
		if err != nil || usage.Call == nil || usage.DebitedTokens != 110 {
			t.Fatalf("unmetered controller: %+v %v", usage, err)
		}
		// Old worker cannot overwrite the committed candidate after cancellation.
		if _, err = op.Cancel(ctx, current.ID, final.Revision); err != nil {
			t.Fatal(err)
		}
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.RenewLease(final.Lease) }); err == nil {
			t.Fatal("cancelled controller still owns lease")
		}
	})
}

func controllerProvenance(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("ControllerRejectsAlteredEvidence", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := candidateBundle(t)
		ctx := scoped(bundle.Experiment.Spec.Scope)
		lease := proposalExperiment(ctx, t, s, bundle)
		experiment, err := s.GetExperiment(ctx, bundle.Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		trials, err := s.ListTrials(ctx, experiment.ID, optimization.TrialQuery{PageQuery: optimization.PageQuery{Limit: 10}, Split: optimization.SplitDiscovery})
		if err != nil || len(trials.Items) == 0 {
			t.Fatal("missing discovery fixture", err)
		}
		trial := trials.Items[0]
		evaluation, err := s.GetEvaluation(ctx, trial.EvaluationID)
		if err != nil {
			t.Fatal(err)
		}
		base, err := s.GetVersion(ctx, experiment.Spec.Baseline.ID)
		if err != nil {
			t.Fatal(err)
		}
		provider := &controllerProvider{body: `{"proposals":[]}`}
		authority := controllerAuthority{actor: experiment.Spec.Requester}
		models, err := optimization.NewControllerFactory(s, authority, authority, []optimization.ModelBinding{{Model: "fixture", Revision: "1", Provider: provider, Counter: controllerCounter{}}}, optimization.ControllerLimits{MaxInputBytes: 1 << 20, MaxResponseBytes: 4096, MaxOutputTokens: 10, MaxProposals: 2, MaxLessons: 2})
		if err != nil {
			t.Fatal(err)
		}
		controller, err := models.ForWork(ctx, optimization.WorkItem{Experiment: experiment, Lease: lease})
		if err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"trial", "evaluation", "round", "lesson"} {
			input := optimization.ProposalInput{Baseline: base, Policy: base.Policy, Round: experiment.Round, Evidence: []optimization.DiscoveryEvidence{{Trial: trial, Evaluation: evaluation}}}
			switch mode {
			case "trial":
				input.Evidence[0].Trial.Revision++
			case "evaluation":
				input.Evidence[0].Evaluation.TrialID = experiment.ID
			case "round":
				input.Round++
			case "lesson":
				input.Lessons = []optimization.Lesson{{Statement: "unfrozen instruction"}}
			}
			if _, err = controller.Propose(ctx, input); err == nil {
				t.Fatalf("accepted altered %s", mode)
			}
		}
		if provider.calls != 0 {
			t.Fatalf("untrusted evidence dispatched %d calls", provider.calls)
		}
	})
}

func controllerReflection(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("WorkerCommitsReflectionPublication", func(t *testing.T) {
		s, ctx, bundle, lease, decision := decisionFixture(t, factory)
		commitTrialChange(ctx, t, s, decisionChange(ctx, t, s, bundle, lease, decision))
		provider := &controllerProvider{body: `{"lessons":[{"statement":"Check available evidence.","domain":"reasoning"}]}`}
		authority := controllerAuthority{actor: bundle.Experiment.Spec.Requester}
		models, err := optimization.NewControllerFactory(s, authority, authority, []optimization.ModelBinding{{Model: "fixture", Revision: "1", Provider: provider, Counter: controllerCounter{}}}, optimization.ControllerLimits{MaxInputBytes: 1 << 20, MaxResponseBytes: 4096, MaxOutputTokens: 10, MaxProposals: 2, MaxLessons: 2})
		if err != nil {
			t.Fatal(err)
		}
		handler, err := optimization.NewControllerPhaseHandler(s, models, nil, "reasoning")
		if err != nil {
			t.Fatal(err)
		}
		op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: handler, Worker: optimization.DefaultWorkerConfig()})
		if err != nil {
			t.Fatal(err)
		}
		current, err := s.GetExperiment(ctx, bundle.Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		paused, err := op.Pause(ctx, current.ID, current.Revision)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = op.Resume(ctx, current.ID, paused.Revision); err != nil {
			t.Fatal(err)
		}
		if err = op.WorkOnce(ctx, current.ID); err != nil {
			t.Fatal(err)
		}
		final, err := s.GetExperiment(ctx, current.ID)
		if err != nil || final.State != optimization.StateReviewRequired || provider.calls != 1 {
			t.Fatalf("reflection completion: %+v %v calls=%d", final, err, provider.calls)
		}
		lessons, err := s.ListLessons(ctx, optimization.LessonQuery{PageQuery: optimization.PageQuery{Limit: 10}})
		if err != nil || len(lessons.Items) != 1 {
			t.Fatalf("reflection lessons: %+v %v", lessons, err)
		}
		lesson := lessons.Items[0]
		publication, err := s.GetPublication(ctx, lesson.ID, lesson.Revision)
		if err != nil || publication.ExperimentID != current.ID {
			t.Fatalf("reflection publication: %+v %v", publication, err)
		}
	})
}
