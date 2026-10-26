package contract

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	dashauth "github.com/xraph/forge/extensions/dashboard/auth"
	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store/memory"
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

func TestOptimizationCreateContractRecovery(t *testing.T) {
	repo, err := memory.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	bundle := storetest.Fixture(t, "create-contract")
	spec := bundle.Experiment.Spec
	catalog := &optimizationCreationCatalog{bundle: bundle}
	auth := &optimizationTestAuthority{}
	service, err := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Caller: auth, Authorizer: auth, IdentityResolver: auth, Handler: auth, Worker: opt.DefaultWorkerConfig(), CreationCatalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	p := dc.Principal{User: &dashauth.UserInfo{Subject: "fixture", Scopes: []string{"cortex.optimization.manage"}}, Claims: map[string]any{"cortex_scope": spec.Scope}}
	auditFailure := false
	audit := func(_ context.Context, event AuditEvent) error {
		if auditFailure && event.Outcome == "succeeded" {
			return errors.New("audit unavailable")
		}
		return nil
	}
	d, registry, wardens, cache := optimizationCommandDispatcher(t, service, spec.Scope, audit)
	selection := opt.CreationSelection{Agent: opt.ContentRef{Ref: spec.AgentID.String(), Revision: "1", Digest: strings.Repeat("a", 64)}, Evaluation: spec.Evaluation, Controller: opt.ContentRef{Ref: spec.Controller.ID.String(), Revision: "1", Digest: spec.Controller.Digest}, Environment: spec.Tasks[0].EnvironmentSnapshot}
	input := map[string]any{"selection": selection, "idempotency_key": "forged", "bundle": map[string]any{"requester": "forged", "policy": "forged"}}
	invoke := func(disp *dispatcher.Dispatcher, principal dc.Principal, key string) (opt.Experiment, error) {
		payload, e := json.Marshal(input)
		if e != nil {
			t.Fatal(e)
		}
		data, _, e := disp.Dispatch(context.Background(), dc.Request{Contributor: ContributorName, Intent: "optimization.experiments.create", IntentVersion: 1, Kind: dc.KindCommand, IdempotencyKey: key, Payload: payload}, principal)
		var result opt.Experiment
		if e == nil {
			if decodeErr := json.Unmarshal(data, &result); decodeErr != nil {
				t.Fatal(decodeErr)
			}
		}
		return result, e
	}
	for _, scopes := range [][]string{nil, {"cortex.manage"}, {"cortex.optimization.read"}, {"cortex.optimization.run"}, {"cortex.optimization.activate"}} {
		denied := p
		denied.User = &dashauth.UserInfo{Subject: "fixture", Scopes: scopes}
		if _, err = invoke(d, denied, "create"); !errors.Is(err, dc.ErrPermissionDenied) {
			t.Fatalf("permission %v: %v", scopes, err)
		}
	}
	if _, err = invoke(d, p, ""); !errors.Is(err, dc.ErrBadRequest) {
		t.Fatalf("payload supplied key: %v", err)
	}
	noAudit, _, _, _ := optimizationCommandDispatcher(t, service, spec.Scope, nil)
	if _, err = invoke(noAudit, p, "create"); !errors.Is(err, dc.ErrUnavailable) {
		t.Fatalf("missing audit: %v", err)
	}
	unavailable, _, _, _ := optimizationCommandDispatcher(t, nil, spec.Scope, audit)
	if _, err = invoke(unavailable, p, "create"); !errors.Is(err, dc.ErrUnavailable) {
		t.Fatalf("missing optimizer: %v", err)
	}
	foreign := p
	foreign.Claims = map[string]any{"cortex_scope": cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "foreign"}}}}
	if _, err = invoke(d, foreign, "create"); !errors.Is(err, dc.ErrPermissionDenied) {
		t.Fatalf("foreign catalog: %v", err)
	}
	if catalog.calls != 0 {
		t.Fatal("rejected command prepared a draft")
	}
	auditFailure = true
	_, err = invoke(d, p, "create")
	var failure *dc.Error
	if !errors.As(err, &failure) || failure.Code != dc.CodeUnavailable || failure.Details["command_attempted"] != true {
		t.Fatalf("lost audit outcome: %v", err)
	}
	auditFailure = false
	catalog.prepareErr = opt.ErrUnavailable
	recovered, err := invoke(d, p, "create")
	if err != nil || recovered.State != opt.StateDraft || recovered.Revision != 1 || recovered.Spec.Requester.Subject != "fixture" || catalog.calls != 1 {
		t.Fatalf("creation recovery: %+v %v calls=%d", recovered, err, catalog.calls)
	}
	auth.denied = true
	if _, err = invoke(d, p, "create"); !errors.Is(err, dc.ErrPermissionDenied) {
		t.Fatalf("revoked creation: %v", err)
	}
	auth.denied = false
	catalog.grantErr = opt.ErrPolicyDenied
	if _, err = invoke(d, p, "create"); !errors.Is(err, dc.ErrPermissionDenied) {
		t.Fatalf("revoked catalog: %v", err)
	}
	catalog.grantErr = nil
	selection.Mode = opt.ModeParallel
	input["selection"] = selection
	if _, err = invoke(d, p, "create"); !errors.Is(err, dc.ErrConflict) {
		t.Fatalf("changed selection: %v", err)
	}
	selection.Mode = ""
	input["selection"] = selection
	invalidates := []string{"optimization.experiments.list", "optimization.experiments.detail", "optimization.capabilities", "optimization.events.list"}
	intent, ok := registry.Intent(ContributorName, "optimization.experiments.create", 1)
	if !ok || !reflect.DeepEqual(intent.Invalidates, invalidates) {
		t.Fatalf("create invalidations: %+v", intent)
	}
	receipt := optimizationHTTPReceipt[opt.Experiment](t, d, registry, wardens, p, "optimization.experiments.create", "create", input, invalidates)
	if !reflect.DeepEqual(receipt, recovered) || cache.calls != 0 {
		t.Fatal("HTTP retry bypassed durable creation")
	}
	ctx := cortex.WithScope(context.Background(), spec.Scope)
	page, err := repo.ListExperiments(ctx, opt.ListQuery{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("duplicate creation: %+v %v", page, err)
	}
}
