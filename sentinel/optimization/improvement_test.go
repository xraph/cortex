package optimization_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/xraph/sentinel"
	se "github.com/xraph/sentinel/engine"
	sid "github.com/xraph/sentinel/id"
	ssqlite "github.com/xraph/sentinel/store/sqlite"
	"github.com/xraph/sentinel/suite"
	"github.com/xraph/sentinel/testcase"

	"github.com/xraph/cortex"
	cid "github.com/xraph/cortex/id"
	opt "github.com/xraph/cortex/optimization"
	osqlite "github.com/xraph/cortex/optimization/store/sqlite"
	"github.com/xraph/cortex/optimization/store/storetest"
	adapter "github.com/xraph/cortex/sentinel/optimization"
)

type creationOnlyHandler struct{ t *testing.T }

func (h creationOnlyHandler) Execute(context.Context, opt.WorkItem) (opt.Transition, error) {
	h.t.Error("comparison creation dispatched work")
	return opt.Transition{}, opt.ErrUnavailable
}

func TestSentinelImprovementCreationSurvivesReopen(t *testing.T) {
	bundle := storetest.Fixture(t, "sentinel-improvement")
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	mapped := sentinel.WithApp(sentinel.WithTenant(ctx, "tenant"), "app")
	dir := t.TempDir()
	optimizerPath, sentinelPath := filepath.Join(dir, "optimizer.db"), filepath.Join(dir, "sentinel.db")
	repository := osqlite.New(loopDB(t, optimizerPath))
	if err := repository.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	sentinelStore := ssqlite.New(loopDB(t, sentinelPath))
	if err := sentinelStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	authority := loopAuthority{actor: bundle.Experiment.Spec.Requester}
	judgeModel := &loopJudgeModel{}
	makeEngine := func() (*se.Engine, *adapter.MeteredJudge) {
		t.Helper()
		judge, err := adapter.NewMeteredJudge(adapter.JudgeConfig{Name: "quality", Repository: repository, Authorizer: authority, IdentityResolver: authority, Model: opt.ModelBinding{Model: "fixture", Revision: "1", Provider: judgeModel, Counter: loopCounter{}, Pricing: loopPricing{}, Reporter: loopReporter{}}, MaxInputBytes: 1 << 20, MaxResponseBytes: 4096, MaxOutputTokens: 100})
		if err != nil {
			t.Fatal(err)
		}
		eng, err := se.New(se.WithStore(sentinelStore))
		if err != nil {
			t.Fatal(err)
		}
		if err = judge.Register(eng.Scorers()); err != nil {
			t.Fatal(err)
		}
		return eng, judge
	}
	eng, judge := makeEngine()
	su := &suite.Suite{ID: sid.NewSuiteID(), AppID: "app", Name: "controller-comparison"}
	if err := sentinelStore.CreateSuite(mapped, su); err != nil {
		t.Fatal(err)
	}
	bindings := make([]adapter.CaseBinding, 0, len(bundle.Experiment.Spec.Tasks))
	for _, task := range bundle.Experiment.Spec.Tasks {
		tc := &testcase.Case{ID: sid.NewCaseID(), SuiteID: su.ID, Name: task.ID, Input: task.Input, Expected: "private answer", Scorers: []testcase.ScorerConfig{{Name: "quality", Config: map[string]any{"rubric": "Score correctness."}}}}
		if err := sentinelStore.CreateCase(mapped, tc); err != nil {
			t.Fatal(err)
		}
		bindings = append(bindings, adapter.CaseBinding{Task: task, CaseID: tc.ID})
	}
	frozen, err := eng.FreezeEvaluation(mapped, se.FreezeRequest{SuiteID: su.ID})
	if err != nil {
		t.Fatal(err)
	}
	ref := opt.ContentRef{Ref: frozen.ID.String(), Revision: "1", Digest: frozen.Digest}
	bundle.Experiment.Spec.Evaluation = ref
	makeEvaluator := func() *adapter.Evaluator {
		t.Helper()
		evaluator, createErr := adapter.NewEvaluator(adapter.Config{Engine: eng, Repository: repository, Judges: []*adapter.MeteredJudge{judge}, Mapper: mapper(func(ctx context.Context, _ cortex.Scope) (context.Context, error) {
			return sentinel.WithApp(sentinel.WithTenant(ctx, "tenant"), "app"), nil
		}), Bindings: []adapter.Binding{{Scope: bundle.Experiment.Spec.Scope, TenantID: "tenant", AppID: "app", Plan: ref, Cases: bindings}}})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return evaluator
	}
	evaluator := makeEvaluator()
	makeCatalogAccess := func(value *adapter.Evaluator) *adapter.CatalogAccess {
		t.Helper()
		access, accessErr := adapter.NewCatalogAccess(value, catalogGrant(func(mapped context.Context, selected opt.ContentRef) error {
			if sentinel.AppFromContext(mapped) != "app" || sentinel.TenantFromContext(mapped) != "tenant" || selected != ref {
				return opt.ErrPolicyDenied
			}
			return nil
		}))
		if accessErr != nil {
			t.Fatal(accessErr)
		}
		choice := opt.CatalogChoice{Scope: bundle.Experiment.Spec.Scope, Kind: opt.CatalogEvaluation, Ref: ref, Name: "Metered evaluation"}
		if accessErr = access.Authorize(ctx, choice); accessErr != nil {
			t.Fatal(accessErr)
		}
		return access
	}
	bundle.Experiment.EvaluationPlan, err = makeCatalogAccess(evaluator).Freeze(ctx, opt.EvaluationSpec{Source: ref, Tasks: bundle.Experiment.Spec.Tasks})
	if err != nil {
		t.Fatal(err)
	}
	candidate := bundle.Versions[1]
	candidate.ID = cid.New(opt.PrefixVersion)
	controller := *candidate.Controller
	controller.ModelRevision = "candidate"
	candidate.Controller = &controller
	candidate, err = opt.SealVersion(candidate)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Versions = append(bundle.Versions, candidate)
	plan := &opt.ImprovementPlan{Controllers: [2]opt.VersionRef{bundle.Experiment.Spec.Controller, {ID: candidate.ID, Digest: candidate.Digest}}}
	for arm := range 2 {
		child := bundle.Experiment.Spec
		child.Controller = plan.Controllers[arm]
		child.Parent = &opt.ParentExperiment{ExperimentID: bundle.Experiment.ID, Arm: arm, Sequence: 0}
		plan.Children = append(plan.Children, opt.ImprovementChild{ExperimentID: cid.New(opt.PrefixExperiment), Spec: child, EvaluationPlan: bundle.Experiment.EvaluationPlan})
	}
	bundle.Experiment.Spec.Improvement = plan
	op, err := opt.NewOptimizer(opt.OptimizerConfig{Repository: repository, Authorizer: authority, IdentityResolver: authority, Caller: authority, ImprovementEvaluator: evaluator, Handler: creationOnlyHandler{t}, Worker: opt.DefaultWorkerConfig()})
	if err != nil {
		t.Fatal(err)
	}
	created, err := op.CreateImprovementExperiment(ctx, opt.ImprovementSpec{Bundle: bundle})
	if err != nil {
		t.Fatal(err)
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
	eng, judge = makeEngine()
	stored, err := repository.GetExperiment(ctx, created.ID)
	if err != nil || stored.State != opt.StateDraft || !reflect.DeepEqual(stored.Spec.Improvement, created.Spec.Improvement) || !reflect.DeepEqual(stored.EvaluationPlan, created.EvaluationPlan) {
		t.Fatalf("changed comparison after reopen: %v", err)
	}
	reopenedPlan, err := makeCatalogAccess(makeEvaluator()).Freeze(ctx, opt.EvaluationSpec{Source: ref, Tasks: stored.Spec.Tasks})
	if err != nil || !reflect.DeepEqual(reopenedPlan, stored.EvaluationPlan) {
		t.Fatalf("changed Sentinel plan after reopen: %v", err)
	}
	reservations, err := repository.ListReservations(ctx, created.ID, opt.PageQuery{Limit: 100})
	if err != nil || len(reservations.Items) != 0 || judgeModel.calls != 0 {
		t.Fatalf("creation billed a model: reservations=%d calls=%d err=%v", len(reservations.Items), judgeModel.calls, err)
	}
}
