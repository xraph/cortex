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
	"github.com/xraph/cortex/optimization/store/storetest"
)

type optimizationCreationCatalog struct {
	bundle               opt.ExperimentBundle
	calls                int
	prepareErr, grantErr error
}

func (c *optimizationCreationCatalog) Authorize(ctx context.Context, _ opt.CreationSelection) error {
	if cortex.ScopeFromContext(ctx).Canonical() != c.bundle.Experiment.Spec.Scope.Canonical() {
		return opt.ErrPolicyDenied
	}
	return c.grantErr
}
func (c *optimizationCreationCatalog) Prepare(_ context.Context, target opt.CreationTarget) (opt.ExperimentBundle, error) {
	c.calls++
	result := c.bundle
	result.Experiment.ID = target.ExperimentID
	result.Experiment.Spec.Requester = target.Actor
	return result, c.prepareErr
}

func TestOptimizationCreateHTTPRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creation.db")
	repo := openOptimizationHTTPStore(t, path)
	t.Cleanup(func() { _ = repo.Close() })
	bundle := storetest.Fixture(t, "create-http")
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	catalog := &optimizationCreationCatalog{bundle: bundle}
	auth := &optimizationAuthority{}
	newHandler := func() http.Handler {
		service, err := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Caller: auth, Authorizer: auth, IdentityResolver: auth, Handler: auth, Worker: opt.DefaultWorkerConfig(), CreationCatalog: catalog})
		if err != nil {
			t.Fatal(err)
		}
		return New(nil, nil, WithOptimization(service)).Handler()
	}
	handler := newHandler()
	spec := bundle.Experiment.Spec
	selection := opt.CreationSelection{Agent: opt.ContentRef{Ref: spec.AgentID.String(), Revision: "1", Digest: strings.Repeat("a", 64)}, Evaluation: spec.Evaluation, Controller: opt.ContentRef{Ref: spec.Controller.ID.String(), Revision: "1", Digest: spec.Controller.Digest}, Environment: spec.Tasks[0].EnvironmentSnapshot}
	input := map[string]any{"selection": selection, "idempotency_key": "forged", "bundle": map[string]any{"requester": "forged", "policy": "forged"}}
	call := func(c context.Context, key string, status int) opt.Experiment {
		response := optimizationPOST(c, t, handler, "/experiments", []string{key}, input)
		if response.Code != status {
			t.Fatalf("creation: %d want %d: %s", response.Code, status, response.Body)
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("creation response can be cached")
		}
		var result opt.Experiment
		if status == 200 {
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	call(ctx, "", 400)
	call(context.Background(), "create", 403)
	call(cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "foreign"}}}), "create", 403)
	auth.denied = true
	call(ctx, "create", 403)
	auth.denied = false
	if catalog.calls != 0 {
		t.Fatal("rejected creation reached preparation")
	}
	unavailable := optimizationPOST(ctx, t, New(nil, nil).Handler(), "/experiments", []string{"create"}, input)
	if unavailable.Code != 503 {
		t.Fatalf("missing optimizer: %d %s", unavailable.Code, unavailable.Body)
	}
	created := call(ctx, "create", 200)
	if created.State != opt.StateDraft || created.ID == bundle.Experiment.ID || created.Spec.Requester.Subject != "fixture" {
		t.Fatalf("draft identity: %+v", created)
	}
	start := optimizationPOST(ctx, t, handler, "/experiments/"+created.ID.String()+"/start", []string{"start"}, map[string]any{"expected_revision": 1})
	if start.Code != 200 {
		t.Fatalf("start: %d %s", start.Code, start.Body)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo = openOptimizationHTTPStore(t, path)
	catalog.prepareErr = opt.ErrUnavailable
	handler = newHandler()
	recovered := call(ctx, "create", 200)
	if recovered.ID != created.ID || recovered.State != opt.StateQueued || recovered.SpecDigest != created.SpecDigest || catalog.calls != 1 {
		t.Fatalf("retry re-prepared or replaced draft: %+v calls=%d", recovered, catalog.calls)
	}
	selection.Mode = opt.ModeParallel
	input["selection"] = selection
	call(ctx, "create", 409)
	selection.Mode = ""
	input["selection"] = selection
	auth.denied = true
	call(ctx, "create", 403)
	auth.denied = false
	catalog.grantErr = opt.ErrPolicyDenied
	call(ctx, "create", 403)
	catalog.grantErr = nil
	page, err := repo.ListExperiments(ctx, opt.ListQuery{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("duplicate drafts: %+v %v", page, err)
	}
	events, err := repo.ListEvents(ctx, created.ID, 0, 100)
	if err != nil || len(events.Items) != 2 {
		t.Fatalf("duplicate creation events: %+v %v", events, err)
	}
}
