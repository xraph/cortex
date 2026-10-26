package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/operator"
	optsqlite "github.com/xraph/cortex/optimization/store/sqlite"
	"github.com/xraph/cortex/optimization/store/storetest"
)

type optimizationAuthority struct{ denied bool }

func (a *optimizationAuthority) Authorize(context.Context, opt.Action, opt.Resource) error {
	if a.denied {
		return fmt.Errorf("SECRET grant: %w", opt.ErrPolicyDenied)
	}
	return nil
}
func (*optimizationAuthority) Current(context.Context) (opt.IdentityRef, error) {
	return opt.IdentityRef{Subject: "fixture"}, nil
}
func (*optimizationAuthority) Resolve(c context.Context, _ opt.IdentityRef, _ cortex.Scope) (context.Context, error) {
	return c, nil
}
func (*optimizationAuthority) Execute(context.Context, opt.WorkItem) (opt.Transition, error) {
	return opt.Transition{}, opt.ErrUnavailable
}

func optimizationHTTPFixture(t *testing.T) (*opt.Optimizer, *optimizationAuthority, opt.ExperimentBundle, context.Context) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "optimization.db")
	open := func() *optsqlite.Store {
		drv := sqlitedriver.New()
		if err := drv.Open(context.Background(), path); err != nil {
			t.Fatal(err)
		}
		db, err := grove.Open(drv)
		if err != nil {
			t.Fatal(err)
		}
		repo := optsqlite.New(db)
		if err = repo.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		return repo
	}
	bundle := storetest.Fixture(t, "http")
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	repo := open()
	if err := repo.Transact(ctx, func(tx opt.Transaction) error { return tx.CreateExperiment(bundle) }); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo = open()
	t.Cleanup(func() { _ = repo.Close() })
	auth := &optimizationAuthority{}
	service, err := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Caller: auth, Authorizer: auth, IdentityResolver: auth, Handler: auth, Worker: opt.DefaultWorkerConfig()})
	if err != nil {
		t.Fatal(err)
	}
	return service, auth, bundle, ctx
}

func optimizationGET(ctx context.Context, handler http.Handler, path string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/optimization"+path, nil))
	return response
}

func TestOptimizationReadHTTP(t *testing.T) {
	service, auth, bundle, ctx := optimizationHTTPFixture(t)
	handler := New(nil, nil, WithOptimization(service)).Handler()
	unavailable := New(nil, nil).Handler()
	experiment := "/experiments/" + bundle.Experiment.ID.String()
	cases := []struct {
		path   string
		status int
	}{
		{"/experiments", 200}, {experiment, 200}, {experiment + "/candidates", 200},
		{experiment + "/candidates/" + id.New(opt.PrefixCandidate).String(), 404},
		{experiment + "/trials", 200}, {experiment + "/trials/" + id.New(opt.PrefixTrial).String(), 404},
		{experiment + "/comparison", 200}, {experiment + "/events", 200},
		{"/versions", 200}, {"/versions/" + bundle.Versions[0].ID.String(), 200},
		{"/lessons", 200}, {"/lessons/" + id.New(opt.PrefixLesson).String(), 404},
		{"/publications?experiment_id=" + bundle.Experiment.ID.String(), 200},
		{"/activations?agent_id=" + bundle.Experiment.Spec.AgentID.String(), 200},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			for _, check := range []struct {
				name    string
				handler http.Handler
				ctx     context.Context
				denied  bool
				status  int
			}{
				{"authorized", handler, ctx, false, tc.status},
				{"denied", handler, ctx, true, 403},
				{"missing scope", handler, context.Background(), false, 403},
				{"unavailable", unavailable, ctx, false, 503},
			} {
				t.Run(check.name, func(t *testing.T) {
					auth.denied = check.denied
					got := optimizationGET(check.ctx, check.handler, tc.path)
					auth.denied = false
					if got.Code != check.status {
						t.Fatalf("status %d want %d: %s", got.Code, check.status, got.Body)
					}
					if check.status >= 400 && !strings.Contains(got.Body.String(), `"code":"optimization_`) {
						t.Fatalf("public error missing: %s", got.Body)
					}
					if strings.Contains(got.Body.String(), "SECRET") {
						t.Fatalf("private error: %s", got.Body)
					}
				})
			}
		})
	}
	for _, path := range []string{"/experiments?limit=-1", "/experiments?limit=101", "/experiments?agent_id=" + bundle.Experiment.ID.String(), experiment + "/trials?version_id=" + bundle.Experiment.ID.String(), "/versions?agent_id=bad", "/experiments/bad", experiment + "/events?cursor=bad"} {
		got := optimizationGET(ctx, handler, path)
		if got.Code != 400 {
			t.Errorf("%s: %d %s", path, got.Code, got.Body)
		}
	}
	foreign := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
	got := optimizationGET(foreign, handler, experiment)
	if got.Code != 404 {
		t.Fatalf("foreign experiment: %d %s", got.Code, got.Body)
	}
	for _, check := range []struct {
		scope  cortex.Scope
		status int
	}{
		{cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: ""}}}, 403},
		{cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "a"}, {Key: "org", Value: "b"}}}, 403},
	} {
		got = optimizationGET(cortex.WithScope(ctx, check.scope), handler, "/experiments")
		if got.Code != check.status {
			t.Fatalf("malformed scope: %d %s", got.Code, got.Body)
		}
	}
}

func TestOptimizationHTTPPagination(t *testing.T) {
	service, _, bundle, ctx := optimizationHTTPFixture(t)
	for range 26 {
		next := storetest.Fixture(t, "http")
		if _, err := service.Create(ctx, next); err != nil {
			t.Fatal(err)
		}
	}
	handler := New(nil, nil, WithOptimization(service)).Handler()
	first := optimizationGET(ctx, handler, "/experiments")
	var page opt.ResourcePage[opt.Experiment]
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if first.Code != 200 || len(page.Items) != 25 || page.Complete || page.NextCursor == "" {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}
	second := optimizationGET(ctx, handler, "/experiments?cursor="+page.NextCursor)
	if err := json.Unmarshal(second.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if second.Code != 200 || len(page.Items) != 2 || !page.Complete {
		t.Fatalf("second: %d %s", second.Code, second.Body)
	}
	filtered := optimizationGET(ctx, handler, "/experiments?agent_id="+bundle.Experiment.Spec.AgentID.String()+"&limit=1")
	if err := json.Unmarshal(filtered.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if filtered.Code != 200 || len(page.Items) != 1 || page.Items[0].ID != bundle.Experiment.ID || !page.Complete {
		t.Fatalf("filter: %d %s", filtered.Code, filtered.Body)
	}
	for _, path := range []string{"/experiments?limit=bad", "/experiments?limit=1&limit=2"} {
		result := optimizationGET(ctx, handler, path)
		if result.Code != 400 {
			t.Fatalf("ambiguous pagination: %d %s", result.Code, result.Body)
		}
	}
	empty := optimizationGET(cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "empty"}}}), handler, "/experiments")
	if empty.Code != 200 || !strings.Contains(empty.Body.String(), `"items":[]`) || !strings.Contains(empty.Body.String(), `"complete":true`) {
		t.Fatalf("empty: %d %s", empty.Code, empty.Body)
	}
	var failure operator.ErrorResponse
	denied := optimizationGET(ctx, New(nil, nil).Handler(), "/experiments")
	if err := json.Unmarshal(denied.Body.Bytes(), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Code != "optimization_unavailable" {
		t.Fatalf("unavailable: %+v", failure)
	}
}
