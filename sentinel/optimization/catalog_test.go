package optimization_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xraph/sentinel"
	se "github.com/xraph/sentinel/engine"
	"github.com/xraph/sentinel/evalplan"
	sid "github.com/xraph/sentinel/id"
	sstore "github.com/xraph/sentinel/store"
	ssqlite "github.com/xraph/sentinel/store/sqlite"
	"github.com/xraph/sentinel/suite"
	"github.com/xraph/sentinel/target"
	"github.com/xraph/sentinel/testcase"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	adapter "github.com/xraph/cortex/sentinel/optimization"
)

type catalogGrant func(context.Context, opt.ContentRef) error

func (f catalogGrant) AuthorizeEvaluation(ctx context.Context, ref opt.ContentRef) error {
	return f(ctx, ref)
}

type catalogPlanSpy struct {
	sstore.Store
	reads     int
	afterRead func()
}

func (s *catalogPlanSpy) GetEvaluationPlan(ctx context.Context, key sid.ID) (*evalplan.Plan, error) {
	s.reads++
	plan, err := s.Store.GetEvaluationPlan(ctx, key)
	if s.afterRead != nil {
		s.afterRead()
	}
	return plan, err
}

type catalogCallerKey struct{}

func TestCatalogAccessGuardsPrivatePlansAndReopen(t *testing.T) {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "project", Value: "catalog"}}}
	ctx := cortex.WithScope(context.WithValue(context.Background(), catalogCallerKey{}, "caller"), scope)
	mapped := sentinel.WithApp(sentinel.WithTenant(ctx, "tenant"), "app")
	path := filepath.Join(t.TempDir(), "sentinel.db")
	source := ssqlite.New(loopDB(t, path))
	if err := source.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	spy := &catalogPlanSpy{Store: source}
	calls := 0
	makeEngine := func() *se.Engine {
		t.Helper()
		eng, err := se.New(se.WithStore(spy), se.WithTarget("must-not-run", "", target.FromFunc("must-not-run", func(context.Context, string) (string, error) { calls++; return "", nil })))
		if err != nil {
			t.Fatal(err)
		}
		return eng
	}
	eng := makeEngine()
	su := &suite.Suite{ID: sid.NewSuiteID(), AppID: "app", Name: "catalog"}
	if err := source.CreateSuite(mapped, su); err != nil {
		t.Fatal(err)
	}
	tc := &testcase.Case{ID: sid.NewCaseID(), SuiteID: su.ID, Name: "case", Input: "question", Expected: "SECRET expected", Scorers: []testcase.ScorerConfig{{Name: "exact"}}}
	if err := source.CreateCase(mapped, tc); err != nil {
		t.Fatal(err)
	}
	frozen, err := eng.FreezeEvaluation(mapped, se.FreezeRequest{SuiteID: su.ID})
	if err != nil {
		t.Fatal(err)
	}
	task, err := opt.SealTaskCase(opt.TaskCase{ID: "case", Input: "question", Split: opt.SplitDiscovery, EnvironmentSnapshot: opt.ContentRef{Ref: "snapshot", Revision: "1", Digest: strings.Repeat("a", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	ref := opt.ContentRef{Ref: frozen.ID.String(), Revision: "1", Digest: frozen.Digest}
	choice := opt.CatalogChoice{Scope: scope, Kind: opt.CatalogEvaluation, Ref: ref, Name: "Evaluation"}
	spec := opt.EvaluationSpec{Source: ref, Tasks: []opt.TaskCase{task}}
	binding := adapter.Binding{Scope: scope, AppID: "app", TenantID: "tenant", Plan: ref, Cases: []adapter.CaseBinding{{Task: task, CaseID: tc.ID}}}
	grantErr := error(nil)
	checks := 0
	grant := catalogGrant(func(c context.Context, r opt.ContentRef) error {
		checks++
		if sentinel.AppFromContext(c) != "app" || sentinel.TenantFromContext(c) != "tenant" || cortex.ScopeFromContext(c).Canonical() != scope.Canonical() || c.Value(catalogCallerKey{}) != "caller" || r != ref {
			t.Error("grant lost mapped identity or caller")
		}
		cortex.ScopeFromContext(c).Levels[0].Value = "mutated"
		return grantErr
	})
	var evaluator *adapter.Evaluator
	makeAccess := func() *adapter.CatalogAccess {
		t.Helper()
		var createErr error
		evaluator, createErr = adapter.NewEvaluator(adapter.Config{Engine: eng, Mapper: mapper(func(c context.Context, _ cortex.Scope) (context.Context, error) {
			return sentinel.WithApp(sentinel.WithTenant(c, "tenant"), "app"), nil
		}), Bindings: []adapter.Binding{binding}})
		if createErr != nil {
			t.Fatal(createErr)
		}
		access, createErr := adapter.NewCatalogAccess(evaluator, grant)
		if createErr != nil {
			t.Fatal(createErr)
		}
		return access
	}
	access := makeAccess()
	for _, phase := range []string{"initial", "reopened"} {
		t.Run(phase, func(t *testing.T) {
			for _, denial := range []error{opt.ErrPolicyDenied, opt.ErrUnavailable} {
				grantErr = denial
				spy.reads = 0
				if got := access.Authorize(ctx, choice); !errors.Is(got, denial) || spy.reads != 0 {
					t.Fatalf("denied discovery read private plan: %v reads=%d", got, spy.reads)
				}
				if _, got := access.Freeze(ctx, spec); !errors.Is(got, denial) || spy.reads != 0 {
					t.Fatalf("denied creation read private plan: %v reads=%d", got, spy.reads)
				}
			}
			registry, registryErr := opt.NewApprovedCatalog(opt.ApprovedCatalogConfig{Choices: []opt.CatalogChoice{choice}, Authorize: access.Authorize})
			if registryErr != nil {
				t.Fatal(registryErr)
			}
			target := opt.CatalogTarget{Scope: scope, Actor: opt.IdentityRef{Subject: "caller"}, Kind: opt.CatalogEvaluation, Limit: 25}
			grantErr = opt.ErrPolicyDenied
			spy.reads = 0
			listed, listErr := registry.List(ctx, target)
			if listErr != nil || len(listed.Page.Items) != 0 || spy.reads != 0 {
				t.Fatalf("revoked registry read: %+v %v reads=%d", listed, listErr, spy.reads)
			}
			grantErr = opt.ErrUnavailable
			if _, listErr = registry.List(ctx, target); !errors.Is(listErr, opt.ErrUnavailable) {
				t.Fatalf("outage became empty catalog: %v", listErr)
			}
			grantErr = nil
			listed, listErr = registry.List(ctx, target)
			if listErr != nil || len(listed.Page.Items) != 1 || listed.Page.Items[0].Ref != ref {
				t.Fatalf("registered plan not qualified: %+v %v", listed, listErr)
			}
			grantErr = nil
			spy.reads = 0
			checks = 0
			if got := access.Authorize(ctx, choice); got != nil || spy.reads != 1 || checks != 2 {
				t.Fatalf("qualified discovery: %v reads=%d checks=%d", got, spy.reads, checks)
			}
			if scope.Levels[0].Value != "catalog" {
				t.Fatal("callback mutated caller scope")
			}
			plan, got := access.Freeze(ctx, spec)
			if got != nil || plan.Ref != ref || len(plan.Criteria) != 1 {
				t.Fatalf("qualified creation: %+v %v", plan, got)
			}
			spy.afterRead = func() { grantErr = opt.ErrPolicyDenied }
			plan, got = access.Freeze(ctx, spec)
			if !errors.Is(got, opt.ErrPolicyDenied) || plan.Ref.Ref != "" {
				t.Fatalf("returned plan after revoked grant: %+v %v", plan, got)
			}
			grantErr = nil
			if got = access.Authorize(ctx, choice); !errors.Is(got, opt.ErrPolicyDenied) {
				t.Fatalf("discovery ignored revoked grant: %v", got)
			}
			spy.afterRead = nil
			grantErr = nil
			bad := spec
			bad.RequiredCriteria = []string{"unregistered"}
			if _, got = access.Freeze(ctx, bad); !errors.Is(got, opt.ErrIncompleteEvidence) {
				t.Fatalf("skipped plan qualification: %v", got)
			}
			for _, kind := range []opt.CatalogKind{opt.CatalogAgent, opt.CatalogController, opt.CatalogEnvironment} {
				badChoice := choice
				badChoice.Kind = kind
				spy.reads = 0
				if got = access.Authorize(ctx, badChoice); !errors.Is(got, opt.ErrPolicyDenied) || spy.reads != 0 {
					t.Fatalf("accepted wrong source kind: %v", got)
				}
			}
			foreign := choice
			foreign.Scope = cortex.Scope{Levels: []cortex.Level{{Key: "project", Value: "other"}}}
			stale := choice
			stale.Ref.Revision = "2"
			for _, badChoice := range []opt.CatalogChoice{foreign, stale} {
				spy.reads = 0
				if got = access.Authorize(ctx, badChoice); !errors.Is(got, opt.ErrPolicyDenied) || spy.reads != 0 {
					t.Fatalf("accepted foreign or stale source: %v", got)
				}
			}
			for _, badCtx := range []context.Context{sentinel.WithApp(ctx, "other"), sentinel.WithTenant(ctx, "other")} {
				spy.reads = 0
				if got = access.Authorize(badCtx, choice); !errors.Is(got, opt.ErrPolicyDenied) || spy.reads != 0 {
					t.Fatalf("accepted conflicting mapping: %v", got)
				}
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			spy.reads = 0
			if got = access.Authorize(cancelled, choice); !errors.Is(got, context.Canceled) || spy.reads != 0 {
				t.Fatalf("ignored cancellation: %v", got)
			}
			if calls != 0 {
				t.Fatal("catalog executed target")
			}
		})
		if phase == "initial" {
			if err = source.Close(); err != nil {
				t.Fatal(err)
			}
			source = ssqlite.New(loopDB(t, path))
			spy.Store = source
			eng = makeEngine()
			access = makeAccess()
		}
	}
	badEvaluator, err := adapter.NewEvaluator(adapter.Config{Engine: eng, Bindings: []adapter.Binding{binding}, Mapper: mapper(func(c context.Context, _ cortex.Scope) (context.Context, error) {
		foreignScope := cortex.ScopeFromContext(c)
		foreignScope.Levels[0].Value = "foreign"
		return sentinel.WithApp(sentinel.WithTenant(cortex.WithScope(c, foreignScope), "tenant"), "app"), nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	badAccess, err := adapter.NewCatalogAccess(badEvaluator, grant)
	if err != nil {
		t.Fatal(err)
	}
	spy.reads = 0
	if err = badAccess.Authorize(ctx, choice); !errors.Is(err, opt.ErrPolicyDenied) || spy.reads != 0 || scope.Levels[0].Value != "catalog" {
		t.Fatalf("mapper mutated caller or read foreign plan: %v reads=%d scope=%+v", err, spy.reads, scope)
	}
	if err = source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = adapter.NewCatalogAccess(nil, grant); !errors.Is(err, opt.ErrInvalidSpec) {
		t.Fatalf("nil evaluator: %v", err)
	}
	if _, err = adapter.NewCatalogAccess(evaluator, nil); !errors.Is(err, opt.ErrInvalidSpec) {
		t.Fatalf("nil authorizer: %v", err)
	}
}
