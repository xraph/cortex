package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/operator"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestOptimizationCatalogHTTPCreationAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	repo := openOptimizationHTTPStore(t, path)
	t.Cleanup(func() { _ = repo.Close() })
	bundle := storetest.Fixture(t, "catalog-http")
	spec := bundle.Experiment.Spec
	ctx := cortex.WithScope(context.Background(), spec.Scope)
	auth := &optimizationAuthority{}
	source := &optimizationCreationCatalog{bundle: bundle}
	selection := opt.CreationSelection{Agent: opt.ContentRef{Ref: spec.AgentID.String(), Revision: "1", Digest: strings.Repeat("a", 64)}, Evaluation: spec.Evaluation, Controller: opt.ContentRef{Ref: spec.Controller.ID.String(), Revision: "1", Digest: spec.Controller.Digest}, Environment: spec.Tasks[0].EnvironmentSnapshot}
	choices := []opt.CatalogChoice{{Scope: spec.Scope, Kind: opt.CatalogAgent, Ref: selection.Agent, Name: "Agent"}, {Scope: spec.Scope, Kind: opt.CatalogEvaluation, Ref: selection.Evaluation, Name: "Evaluation"}, {Scope: spec.Scope, Kind: opt.CatalogController, Ref: selection.Controller, Name: "Controller"}, {Scope: spec.Scope, Kind: opt.CatalogEnvironment, Ref: selection.Environment, Name: "Environment"}}
	evaluationDenied := false
	catalog, err := opt.NewApprovedCatalog(opt.ApprovedCatalogConfig{Choices: choices, Authorize: func(_ context.Context, choice opt.CatalogChoice) error {
		if evaluationDenied && choice.Kind == opt.CatalogEvaluation {
			return opt.ErrPolicyDenied
		}
		return source.grantErr
	}, Prepare: source.Prepare})
	if err != nil {
		t.Fatal(err)
	}
	newHandler := func(provider opt.CatalogProvider) http.Handler {
		service, e := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Caller: auth, Authorizer: auth, IdentityResolver: auth, Handler: auth, Worker: opt.DefaultWorkerConfig(), Catalog: provider, CreationCatalog: catalog})
		if e != nil {
			t.Fatal(e)
		}
		return New(nil, nil, WithOptimization(service)).Handler()
	}
	handler := newHandler(catalog)
	read := func(c context.Context, path string, status int) opt.ResourcePage[opt.CatalogChoice] {
		response := optimizationGET(c, handler, "/catalog"+path)
		if response.Code != status {
			t.Fatalf("catalog %s: %d want %d: %s", path, response.Code, status, response.Body)
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("catalog response can be cached")
		}
		var page opt.ResourcePage[opt.CatalogChoice]
		if status == 200 {
			if e := json.Unmarshal(response.Body.Bytes(), &page); e != nil {
				t.Fatal(e)
			}
		}
		return page
	}
	for _, query := range []string{"?kind=invalid", "?limit=101", "?kind=agent&kind=environment", "?cursor=bad"} {
		read(ctx, query, 400)
	}
	read(context.Background(), "", 403)
	auth.denied = true
	read(ctx, "", 403)
	auth.denied = false
	first := read(ctx, "?limit=2", 200)
	if len(first.Items) != 2 || first.Complete {
		t.Fatalf("first page: %+v", first)
	}
	second := read(ctx, "?limit=2&cursor="+first.NextCursor, 200)
	if len(second.Items) != 2 || !second.Complete {
		t.Fatalf("second page: %+v", second)
	}
	foreign := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "foreign"}}})
	read(foreign, "?cursor="+first.NextCursor, 400)
	if page := read(foreign, "", 200); len(page.Items) != 0 || !page.Complete {
		t.Fatal("foreign catalog leaked choices")
	}
	if source.calls != 0 {
		t.Fatal("listing prepared an experiment")
	}
	response := optimizationPOST(ctx, t, handler, "/experiments", []string{"from-catalog"}, operator.CreateInput{Selection: selection})
	if response.Code != 200 || source.calls != 1 {
		t.Fatalf("approved creation: %d %s calls=%d", response.Code, response.Body, source.calls)
	}
	evaluationDenied = true
	if err = repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo = openOptimizationHTTPStore(t, path)
	handler = newHandler(catalog)
	if page := read(ctx, "?kind=evaluation", 200); len(page.Items) != 0 || !page.Complete {
		t.Fatal("restart restored revoked source access")
	}
	response = optimizationPOST(ctx, t, handler, "/experiments", []string{"from-catalog"}, operator.CreateInput{Selection: selection})
	if response.Code != 403 || source.calls != 1 {
		t.Fatalf("creation retry bypassed mapped access: %d %s", response.Code, response.Body)
	}
	evaluationDenied = false
	source.grantErr = opt.ErrUnavailable
	read(ctx, "", 503)
	source.grantErr = nil
	handler = newHandler(nil)
	read(ctx, "", 503)
}
