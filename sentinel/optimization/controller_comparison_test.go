package optimization_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/xraph/sentinel"
	se "github.com/xraph/sentinel/engine"
	"github.com/xraph/sentinel/evalplan"
	sid "github.com/xraph/sentinel/id"
	ssqlite "github.com/xraph/sentinel/store/sqlite"
	"github.com/xraph/sentinel/suite"
	"github.com/xraph/sentinel/testcase"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/llm"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	osqlite "github.com/xraph/cortex/optimization/store/sqlite"
	"github.com/xraph/cortex/optimization/store/storetest"
	adapter "github.com/xraph/cortex/sentinel/optimization"
)

type controllerJudgeModel struct {
	loopJudgeModel
	incomplete bool
}

func (m *controllerJudgeModel) Complete(ctx context.Context, request *llm.Request) (*llm.Response, error) {
	response, err := m.loopJudgeModel.Complete(ctx, request)
	if err == nil && m.incomplete && m.calls == 1 {
		response.Content = "invalid judge output"
	}
	return response, err
}

func TestControllerComparisonPreservesPlansAcrossReopen(t *testing.T) {
	t.Run("complete-scores", func(t *testing.T) { controllerComparisonReopen(t, false) })
	t.Run("incomplete-score", func(t *testing.T) { controllerComparisonReopen(t, true) })
}

func controllerComparisonReopen(t *testing.T, incomplete bool) {
	t.Helper()
	dir := t.TempDir()
	optimizerPath, sentinelPath := filepath.Join(dir, "optimizer.db"), filepath.Join(dir, "sentinel.db")
	repository := osqlite.New(loopDB(t, optimizerPath))
	sentinelStore := ssqlite.New(loopDB(t, sentinelPath))
	ctx := context.Background()
	if err := repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sentinelStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	eng, err := se.New(se.WithStore(sentinelStore))
	if err != nil {
		t.Fatal(err)
	}
	var authority loopAuthority
	judgeModel := &controllerJudgeModel{incomplete: incomplete}
	var judge *adapter.MeteredJudge
	registerJudge := func() {
		var judgeErr error
		judge, judgeErr = adapter.NewMeteredJudge(adapter.JudgeConfig{Name: "quality", Repository: repository, Authorizer: authority, IdentityResolver: authority, Model: opt.ModelBinding{Model: "fixture", Revision: "1", Provider: judgeModel, Counter: loopCounter{}, Pricing: loopPricing{}, Reporter: loopReporter{}}, MaxInputBytes: 1 << 20, MaxResponseBytes: 4096, MaxOutputTokens: 100})
		if judgeErr != nil {
			t.Fatal(judgeErr)
		}
		if judgeErr = judge.Register(eng.Scorers()); judgeErr != nil {
			t.Fatal(judgeErr)
		}
	}
	var bindings []adapter.Binding
	var evaluator *adapter.Evaluator
	makeEvaluator := func() *adapter.Evaluator {
		value, createErr := adapter.NewEvaluator(adapter.Config{Engine: eng, Repository: repository, Judges: []*adapter.MeteredJudge{judge}, Bindings: bindings, Mapper: mapper(func(ctx context.Context, _ cortex.Scope) (context.Context, error) {
			return sentinel.WithApp(sentinel.WithTenant(ctx, "tenant"), "app"), nil
		})})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return value
	}
	configure := func(parent *opt.ExperimentBundle, children []opt.ExperimentBundle) {
		ctx = cortex.WithScope(ctx, parent.Experiment.Spec.Scope)
		authority = loopAuthority{actor: parent.Experiment.Spec.Requester}
		registerJudge()
		parent.Experiment.Spec.Budget.MaxTokens = 10000
		for index := range children {
			children[index].Experiment.Spec.Budget.MaxTokens = 1000
		}
		mapped := sentinel.WithApp(sentinel.WithTenant(ctx, "tenant"), "app")
		freeze := func(bundle *opt.ExperimentBundle) {
			su := &suite.Suite{ID: sid.NewSuiteID(), AppID: "app", Name: "controller-stream-" + sid.NewSuiteID().String()}
			if createErr := sentinelStore.CreateSuite(mapped, su); createErr != nil {
				t.Fatal(createErr)
			}
			binding := adapter.Binding{Scope: bundle.Experiment.Spec.Scope, AppID: "app", TenantID: "tenant"}
			for _, task := range bundle.Experiment.Spec.Tasks {
				tc := &testcase.Case{ID: sid.NewCaseID(), SuiteID: su.ID, Name: task.ID, Input: task.Input, Expected: "answer"}
				if createErr := sentinelStore.CreateCase(mapped, tc); createErr != nil {
					t.Fatal(createErr)
				}
				binding.Cases = append(binding.Cases, adapter.CaseBinding{Task: task, CaseID: tc.ID})
			}
			frozen, freezeErr := eng.FreezeEvaluation(mapped, se.FreezeRequest{SuiteID: su.ID, Scorers: []evalplan.Criterion{{ID: "quality", Scorer: "quality", Config: map[string]any{"rubric": "Score correctness."}}}})
			if freezeErr != nil {
				t.Fatal(freezeErr)
			}
			binding.Plan = opt.ContentRef{Ref: frozen.ID.String(), Revision: "1", Digest: frozen.Digest}
			bindings = append(bindings, binding)
			evaluator = makeEvaluator()
			bundle.Experiment.Spec.Evaluation = binding.Plan
			bundle.Experiment.EvaluationPlan, freezeErr = evaluator.Freeze(ctx, opt.EvaluationSpec{Source: binding.Plan, Tasks: bundle.Experiment.Spec.Tasks})
			if freezeErr != nil {
				t.Fatal(freezeErr)
			}
		}
		freeze(parent)
		for index := 0; index < len(children); index += 2 {
			freeze(&children[index])
			children[index+1].Experiment.Spec.Evaluation = children[index].Experiment.Spec.Evaluation
			children[index+1].Experiment.EvaluationPlan = children[index].Experiment.EvaluationPlan
		}
		for index, child := range children {
			parent.Experiment.Spec.Improvement.Children[index] = opt.ImprovementChild{ExperimentID: child.Experiment.ID, Spec: child.Experiment.Spec, EvaluationPlan: child.Experiment.EvaluationPlan}
		}
	}
	parent, allocation, evidence := storetest.ControllerComparisonFixture(t, func(*testing.T, func() time.Time) store.Store { return repository }, configure, func(ctx context.Context, r opt.EvaluationRequest) (opt.Evaluation, error) {
		return evaluator.Evaluate(ctx, r)
	})
	request, err := opt.BuildControllerComparison(parent, allocation, evidence)
	if err != nil {
		t.Fatal(err)
	}
	calls := judgeModel.calls
	if calls != 12 {
		t.Fatalf("judge dispatches=%d, want 12", calls)
	}
	first, err := evaluator.CompareControllers(ctx, request)
	expectedCases := 2
	if incomplete {
		expectedCases = 0
	}
	if err != nil || len(first.Plans) != 2 || first.PlanDigest != "" || len(first.Cases) != expectedCases || first.CostRatio != nil {
		t.Fatalf("controller comparison: %+v %v", first, err)
	}
	if err = repository.Close(); err != nil {
		t.Fatal(err)
	}
	if err = sentinelStore.Close(); err != nil {
		t.Fatal(err)
	}
	repository = osqlite.New(loopDB(t, optimizerPath))
	sentinelStore = ssqlite.New(loopDB(t, sentinelPath))
	t.Cleanup(func() { _ = repository.Close(); _ = sentinelStore.Close() })
	eng, err = se.New(se.WithStore(sentinelStore))
	if err != nil {
		t.Fatal(err)
	}
	registerJudge()
	evaluator = makeEvaluator()
	second, err := evaluator.CompareControllers(ctx, request)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("reopen changed comparison: %+v %v", second, err)
	}
	handler, handlerErr := opt.NewImprovementPhaseHandler(opt.OptimizerConfig{Repository: repository, Authorizer: authority, IdentityResolver: authority, Caller: authority, ImprovementEvaluator: evaluator, Handler: creationOnlyHandler{t}, Worker: opt.DefaultWorkerConfig()})
	if handlerErr != nil {
		t.Fatal(handlerErr)
	}
	transition, executeErr := handler.Execute(ctx, opt.WorkItem{Experiment: parent, Lease: parent.Lease})
	if executeErr != nil || transition.Decision == nil {
		t.Fatalf("parent Sentinel decision: %+v %v", transition, executeErr)
	}
	if validationErr := opt.ValidateControllerDecisionEvidence(parent, allocation, *transition.Decision, evidence); validationErr != nil {
		t.Fatal(validationErr)
	}
	transition.ExperimentID, transition.ExpectedRevision, transition.Lease, transition.Actor, transition.IdempotencyKey = parent.ID, parent.Revision, parent.Lease, parent.Spec.Requester, "sentinel-parent-decision"
	// Keep the parent at Decide until the request-denial checks finish.
	changed := request
	changed.Groups = append([]opt.ControllerComparisonGroup(nil), request.Groups...)
	changed.Groups[0].BaselineIDs = request.Groups[1].BaselineIDs
	if _, err = evaluator.CompareControllers(ctx, changed); err == nil {
		t.Fatal("accepted substituted child jobs")
	}
	changed = request
	changed.SpecDigest = "changed"
	if _, err = evaluator.CompareControllers(ctx, changed); err == nil {
		t.Fatal("accepted changed parent")
	}
	foreign := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "project", Value: "foreign"}}})
	if _, err = evaluator.CompareControllers(foreign, request); err == nil {
		t.Fatal("accepted foreign scope")
	}
	if err = repository.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(transition) }); err != nil {
		t.Fatal(err)
	}
	if judgeModel.calls != calls {
		t.Fatal("comparison replayed metered judges")
	}
	stored, getErr := repository.GetDecision(ctx, transition.Decision.ID)
	if getErr != nil || !reflect.DeepEqual(stored.Comparison, second) {
		t.Fatalf("persisted Sentinel decision: %+v %v", stored, getErr)
	}
	current, getErr := repository.GetExperiment(ctx, parent.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	review, reviewErr := handler.Execute(ctx, opt.WorkItem{Experiment: current, Lease: current.Lease})
	if reviewErr != nil || review.NextState != opt.StateReviewRequired || len(review.Publications) != 0 {
		t.Fatalf("parent Sentinel review: %+v %v", review, reviewErr)
	}
	review.ExperimentID, review.ExpectedRevision, review.Lease, review.Actor, review.IdempotencyKey = current.ID, current.Revision, current.Lease, current.Spec.Requester, "sentinel-parent-review"
	if err = repository.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(review) }); err != nil {
		t.Fatal(err)
	}
	if err = repository.Close(); err != nil {
		t.Fatal(err)
	}
	repository = osqlite.New(loopDB(t, optimizerPath))
	// A lost review acknowledgment must reuse the persisted transition.
	if err = repository.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(review) }); err != nil {
		t.Fatalf("review retry after reopen: %v", err)
	}
	finished, getErr := repository.GetExperiment(ctx, parent.ID)
	if getErr != nil || finished.State != opt.StateReviewRequired || finished.DecisionID != stored.ID || finished.Revision != current.Revision+1 {
		t.Fatalf("reopened Sentinel review: %+v %v", finished, getErr)
	}
	if judgeModel.calls != calls {
		t.Fatal("review recovery replayed metered judges")
	}
}
