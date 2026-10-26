package optimization_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/storetest"
)

type catalogProvider struct {
	list      func(context.Context, opt.CatalogTarget) (opt.CatalogResult, error)
	authorize func(context.Context, opt.CatalogChoice) error
}

func (p catalogProvider) List(ctx context.Context, target opt.CatalogTarget) (opt.CatalogResult, error) {
	return p.list(ctx, target)
}
func (p catalogProvider) AuthorizeChoice(ctx context.Context, choice opt.CatalogChoice) error {
	if p.authorize != nil {
		return p.authorize(ctx, choice)
	}
	return nil
}

func catalogService(t *testing.T, provider opt.CatalogProvider) (*opt.Optimizer, context.Context, *opt.IdentityRef, *bool) {
	t.Helper()
	repo, err := memory.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	actor := opt.IdentityRef{Subject: "operator"}
	denied := false
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "catalog"}}})
	service, err := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Catalog: provider, Caller: callerIdentity(func(context.Context) (opt.IdentityRef, error) { return actor, nil }), Authorizer: meterAuthorizer(func(context.Context, opt.Action, opt.Resource) error {
		if denied {
			return opt.ErrPolicyDenied
		}
		return nil
	}), IdentityResolver: meterResolver(func(c context.Context, _ opt.IdentityRef, _ cortex.Scope) (context.Context, error) { return c, nil }), Handler: phaseHandler(func(context.Context, opt.WorkItem) (opt.Transition, error) {
		return opt.Transition{}, opt.ErrUnavailable
	}), Worker: opt.DefaultWorkerConfig()})
	if err != nil {
		t.Fatal(err)
	}
	return service, ctx, &actor, &denied
}
func catalogChoice(scope cortex.Scope, ref string) opt.CatalogChoice {
	return opt.CatalogChoice{Scope: scope, Kind: opt.CatalogEnvironment, Ref: opt.ContentRef{Ref: ref, Revision: "1", Digest: strings.Repeat("a", 64)}, Name: ref}
}

func TestCatalogCurrentAuthorityAndCursorBinding(t *testing.T) {
	calls := 0
	var onList func()
	provider := catalogProvider{list: func(ctx context.Context, target opt.CatalogTarget) (opt.CatalogResult, error) {
		calls++
		if onList != nil {
			onList()
		}
		ref, next := "first", "native-second"
		if target.Cursor != "" {
			if target.Cursor != "native-second" {
				t.Fatal("provider received transport cursor")
			}
			ref, next = "second", ""
		}
		return opt.CatalogResult{Target: target, Page: opt.Page[opt.CatalogChoice]{Items: []opt.CatalogChoice{catalogChoice(cortex.ScopeFromContext(ctx), ref)}, NextCursor: next}}, nil
	}}
	service, ctx, actor, denied := catalogService(t, provider)
	first, err := service.ListCatalog(ctx, opt.CatalogQuery{PageQuery: opt.PageQuery{Limit: 1}, Kind: opt.CatalogEnvironment})
	if err != nil || len(first.Items) != 1 || first.Complete || first.NextCursor == "native-second" {
		t.Fatalf("first: %+v %v", first, err)
	}
	query := opt.CatalogQuery{PageQuery: opt.PageQuery{Limit: 1, Cursor: first.NextCursor}, Kind: opt.CatalogEnvironment}
	for _, mode := range []string{"scope", "actor", "kind", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			c, q := ctx, query
			switch mode {
			case "scope":
				c = cortex.WithScope(c, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "foreign"}}})
			case "actor":
				actor.Subject = "other"
			case "kind":
				q.Kind = opt.CatalogEvaluation
			case "malformed":
				q.Cursor = "invalid"
			}
			before := calls
			if _, e := service.ListCatalog(c, q); !errors.Is(e, opt.ErrInvalidSpec) {
				t.Fatalf("cursor rebinding: %v", e)
			}
			if calls != before {
				t.Fatal("invalid continuation reached source")
			}
			actor.Subject = "operator"
		})
	}
	second, err := service.ListCatalog(ctx, query)
	if err != nil || !second.Complete || second.Items[0].Ref.Ref != "second" {
		t.Fatalf("second: %+v %v", second, err)
	}
	onList = func() { *denied = true }
	if _, err = service.ListCatalog(ctx, opt.CatalogQuery{}); !errors.Is(err, opt.ErrPolicyDenied) {
		t.Fatalf("revoked after list: %v", err)
	}
	*denied = false
	onList = func() { actor.Subject = "changed" }
	if _, err = service.ListCatalog(ctx, opt.CatalogQuery{}); !errors.Is(err, opt.ErrPolicyDenied) {
		t.Fatalf("changed actor: %v", err)
	}
	actor.Subject = "operator"
	onList = nil
	if _, err = service.ListCatalog(context.Background(), opt.CatalogQuery{}); !errors.Is(err, cortex.ErrNoScope) {
		t.Fatalf("missing scope: %v", err)
	}
	empty, emptyCtx, _, _ := catalogService(t, nil)
	if _, err = empty.ListCatalog(emptyCtx, opt.CatalogQuery{}); !errors.Is(err, opt.ErrUnavailable) {
		t.Fatalf("missing catalog became empty: %v", err)
	}
}

func TestCatalogRejectsInvalidProviderEvidence(t *testing.T) {
	for _, mode := range []string{"scope", "target", "kind", "duplicate", "oversized", "bad-ref", "stalled-cursor", "revoked-source", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			provider := catalogProvider{list: func(ctx context.Context, target opt.CatalogTarget) (opt.CatalogResult, error) {
				result := opt.CatalogResult{Target: target, Page: opt.Page[opt.CatalogChoice]{Items: []opt.CatalogChoice{catalogChoice(cortex.ScopeFromContext(ctx), "environment")}}}
				switch mode {
				case "scope":
					result.Page.Items[0].Scope = cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "foreign"}}}
				case "target":
					result.Target.Actor.Subject = "other"
				case "kind":
					result.Page.Items[0].Kind = opt.CatalogEvaluation
				case "duplicate":
					result.Page.Items = append(result.Page.Items, result.Page.Items[0])
				case "oversized":
					result.Page.Items[0].Description = strings.Repeat("x", 4096)
				case "bad-ref":
					result.Page.Items[0].Ref.Digest = "bad"
				case "stalled-cursor":
					result.Page.NextCursor = target.Cursor
				}
				return result, nil
			}, authorize: func(context.Context, opt.CatalogChoice) error {
				if mode == "revoked-source" {
					return opt.ErrPolicyDenied
				}
				return nil
			}}
			service, ctx, _, _ := catalogService(t, provider)
			query := opt.CatalogQuery{Kind: opt.CatalogEnvironment}
			if mode == "stalled-cursor" {
				provider.list = func(ctx context.Context, target opt.CatalogTarget) (opt.CatalogResult, error) {
					return opt.CatalogResult{Target: target, Page: opt.Page[opt.CatalogChoice]{Items: []opt.CatalogChoice{catalogChoice(cortex.ScopeFromContext(ctx), "env")}, NextCursor: "native"}}, nil
				}
				service, ctx, _, _ = catalogService(t, provider)
				first, err := service.ListCatalog(ctx, query)
				if err != nil {
					t.Fatal(err)
				}
				query.Cursor = first.NextCursor
			}
			if mode == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if result, err := service.ListCatalog(ctx, query); err == nil || len(result.Items) != 0 {
				t.Fatalf("invalid provider response escaped: %+v %v", result, err)
			}
		})
	}
}

func TestApprovedCatalogFiltersAndFreezesRegistration(t *testing.T) {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "catalog"}}}
	choices := []opt.CatalogChoice{catalogChoice(scope, "one"), catalogChoice(scope, "two"), catalogChoice(scope, "denied"), catalogChoice(cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "foreign"}}}, "foreign")}
	failed := false
	registry, err := opt.NewApprovedCatalog(opt.ApprovedCatalogConfig{Choices: choices, Authorize: func(_ context.Context, choice opt.CatalogChoice) error {
		if failed {
			return opt.ErrUnavailable
		}
		if choice.Ref.Ref == "denied" {
			return opt.ErrPolicyDenied
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	choices[0].Name = "MUTATED"
	service, ctx, _, _ := catalogService(t, registry)
	first, err := service.ListCatalog(ctx, opt.CatalogQuery{PageQuery: opt.PageQuery{Limit: 1}})
	if err != nil || len(first.Items) != 1 || first.Complete || first.Items[0].Name == "MUTATED" {
		t.Fatalf("first registry page: %+v %v", first, err)
	}
	second, err := service.ListCatalog(ctx, opt.CatalogQuery{PageQuery: opt.PageQuery{Limit: 1, Cursor: first.NextCursor}})
	if err != nil || len(second.Items) != 1 || !second.Complete || reflect.DeepEqual(second.Items, first.Items) {
		t.Fatalf("second registry page: %+v %v", second, err)
	}
	first.Items[0].Name = "response mutation"
	again, err := service.ListCatalog(ctx, opt.CatalogQuery{PageQuery: opt.PageQuery{Limit: 1}})
	if err != nil || again.Items[0].Name == "response mutation" {
		t.Fatal("response mutated registry")
	}
	failed = true
	if _, err = service.ListCatalog(ctx, opt.CatalogQuery{}); !errors.Is(err, opt.ErrUnavailable) {
		t.Fatalf("source failure became empty: %v", err)
	}
}

func TestApprovedCatalogCreationUsesRegisteredRevisions(t *testing.T) {
	bundle := storetest.Fixture(t, "approved-catalog")
	spec := bundle.Experiment.Spec
	selection := opt.CreationSelection{Agent: opt.ContentRef{Ref: spec.AgentID.String(), Revision: "1", Digest: strings.Repeat("a", 64)}, Evaluation: spec.Evaluation, Controller: opt.ContentRef{Ref: spec.Controller.ID.String(), Revision: "1", Digest: spec.Controller.Digest}, Environment: spec.Tasks[0].EnvironmentSnapshot}
	choices := []opt.CatalogChoice{{Scope: spec.Scope, Kind: opt.CatalogAgent, Ref: selection.Agent, Name: "Agent"}, {Scope: spec.Scope, Kind: opt.CatalogEvaluation, Ref: selection.Evaluation, Name: "Evaluation"}, {Scope: spec.Scope, Kind: opt.CatalogController, Ref: selection.Controller, Name: "Controller"}, {Scope: spec.Scope, Kind: opt.CatalogEnvironment, Ref: selection.Environment, Name: "Environment"}}
	calls := 0
	registry, err := opt.NewApprovedCatalog(opt.ApprovedCatalogConfig{Choices: choices, Authorize: func(context.Context, opt.CatalogChoice) error { return nil }, Prepare: func(_ context.Context, target opt.CreationTarget) (opt.ExperimentBundle, error) {
		calls++
		bundle.Experiment.ID = target.ExperimentID
		return bundle, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := cortex.WithScope(context.Background(), spec.Scope)
	if err = registry.Authorize(ctx, selection); err != nil {
		t.Fatal(err)
	}
	target := opt.CreationTarget{Scope: spec.Scope, Actor: spec.Requester, ExperimentID: bundle.Experiment.ID, Selection: selection}
	readOnly, err := opt.NewApprovedCatalog(opt.ApprovedCatalogConfig{Choices: choices, Authorize: func(context.Context, opt.CatalogChoice) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = readOnly.Prepare(ctx, target); !errors.Is(err, opt.ErrUnavailable) {
		t.Fatalf("unconfigured preparation: %v", err)
	}
	if _, err = registry.Prepare(ctx, target); err != nil || calls != 1 {
		t.Fatalf("approved preparation: %v calls=%d", err, calls)
	}
	target.Selection.Evaluation.Revision = "unregistered"
	if _, err = registry.Prepare(ctx, target); !errors.Is(err, opt.ErrPolicyDenied) || calls != 1 {
		t.Fatalf("unknown revision prepared: %v calls=%d", err, calls)
	}
	duplicates := make([]opt.CatalogChoice, len(choices)+1)
	copy(duplicates, choices)
	duplicates[len(choices)] = choices[0]
	if _, err = opt.NewApprovedCatalog(opt.ApprovedCatalogConfig{Choices: duplicates, Authorize: func(context.Context, opt.CatalogChoice) error { return nil }}); !errors.Is(err, opt.ErrInvalidSpec) {
		t.Fatalf("duplicate registration: %v", err)
	}
}

func TestCatalogDetachesAuthorizationInputs(t *testing.T) {
	provider := catalogProvider{list: func(ctx context.Context, target opt.CatalogTarget) (opt.CatalogResult, error) {
		return opt.CatalogResult{Target: target, Page: opt.Page[opt.CatalogChoice]{Items: []opt.CatalogChoice{catalogChoice(cortex.ScopeFromContext(ctx), "environment")}}}, nil
	}, authorize: func(ctx context.Context, choice opt.CatalogChoice) error {
		choice.Scope.Levels[0].Value = "mutated"
		scope := cortex.ScopeFromContext(ctx)
		scope.Levels[0].Value = "also-mutated"
		return nil
	}}
	service, ctx, _, _ := catalogService(t, provider)
	result, err := service.ListCatalog(ctx, opt.CatalogQuery{})
	if err != nil || len(result.Items) != 1 || result.Items[0].Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		t.Fatalf("authorization mutated returned scope: %+v %v", result, err)
	}
}

func TestApprovedCatalogEmptyAndChangedPages(t *testing.T) {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "catalog"}}}
	config := opt.ApprovedCatalogConfig{Authorize: func(context.Context, opt.CatalogChoice) error { return nil }}
	empty, err := opt.NewApprovedCatalog(config)
	if err != nil {
		t.Fatal(err)
	}
	service, ctx, _, _ := catalogService(t, empty)
	page, err := service.ListCatalog(ctx, opt.CatalogQuery{})
	if err != nil || page.Items == nil || len(page.Items) != 0 || !page.Complete {
		t.Fatalf("installed empty catalog: %+v %v", page, err)
	}
	config.Choices = []opt.CatalogChoice{catalogChoice(scope, "one"), catalogChoice(scope, "two")}
	before, err := opt.NewApprovedCatalog(config)
	if err != nil {
		t.Fatal(err)
	}
	service, ctx, _, _ = catalogService(t, before)
	first, err := service.ListCatalog(ctx, opt.CatalogQuery{PageQuery: opt.PageQuery{Limit: 1}})
	if err != nil {
		t.Fatal(err)
	}
	config.Choices[0].Ref.Revision = "2"
	after, err := opt.NewApprovedCatalog(config)
	if err != nil {
		t.Fatal(err)
	}
	service, ctx, _, _ = catalogService(t, after)
	if _, err = service.ListCatalog(ctx, opt.CatalogQuery{PageQuery: opt.PageQuery{Cursor: first.NextCursor}}); !errors.Is(err, opt.ErrConflict) {
		t.Fatalf("mixed catalog revisions: %v", err)
	}
}

func TestCatalogInvalidQueryDoesNotReachSource(t *testing.T) {
	provider := catalogProvider{list: func(context.Context, opt.CatalogTarget) (opt.CatalogResult, error) {
		t.Fatal("invalid query reached source")
		return opt.CatalogResult{}, nil
	}}
	service, ctx, _, _ := catalogService(t, provider)
	for _, query := range []opt.CatalogQuery{{Kind: "invalid"}, {PageQuery: opt.PageQuery{Limit: -1}}, {PageQuery: opt.PageQuery{Limit: 101}}, {PageQuery: opt.PageQuery{Cursor: strings.Repeat("a", 8193)}}} {
		if _, err := service.ListCatalog(ctx, query); !errors.Is(err, opt.ErrInvalidSpec) {
			t.Fatalf("invalid query: %v", err)
		}
	}
}
