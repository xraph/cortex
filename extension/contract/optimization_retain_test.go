package contract

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	dashauth "github.com/xraph/forge/extensions/dashboard/auth"
	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestOptimizationRetainContractRecovery(t *testing.T) {
	repo, ctx, change := storetest.ReviewedActivationFixture(t, func(t *testing.T, now func() time.Time) store.Store {
		s, e := memory.New(memory.WithClock(now))
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
	decision, err := repo.GetDecision(ctx, change.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := repo.GetExperiment(ctx, decision.ExperimentID)
	if err != nil {
		t.Fatal(err)
	}
	auth := &optimizationTestAuthority{}
	service, err := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Caller: auth, Authorizer: auth, IdentityResolver: auth, Handler: auth, Worker: opt.DefaultWorkerConfig()})
	if err != nil {
		t.Fatal(err)
	}
	p := dc.Principal{User: &dashauth.UserInfo{Subject: "fixture", Scopes: []string{"cortex.optimization.manage"}}, Claims: map[string]any{"cortex_scope": cortex.ScopeFromContext(ctx)}}
	auditFailure := false
	audit := func(_ context.Context, event AuditEvent) error {
		if auditFailure && event.Outcome == "succeeded" {
			return errors.New("audit unavailable")
		}
		return nil
	}
	d, registry, wardens, cache := optimizationCommandDispatcher(t, service, cortex.ScopeFromContext(ctx), audit)
	input := map[string]any{"id": current.ID.String(), "expected_revision": current.Revision, "decision_id": decision.ID.String(), "reason": "Keep current baseline", "idempotency_key": "forged"}
	invoke := func(disp *dispatcher.Dispatcher, principal dc.Principal, key string) (opt.Experiment, error) {
		payload, e := json.Marshal(input)
		if e != nil {
			t.Fatal(e)
		}
		data, _, e := disp.Dispatch(context.Background(), dc.Request{Contributor: ContributorName, Intent: "optimization.decisions.retain", IntentVersion: 1, Kind: dc.KindCommand, IdempotencyKey: key, Payload: payload}, principal)
		var result opt.Experiment
		if e == nil {
			if decodeErr := json.Unmarshal(data, &result); decodeErr != nil {
				t.Fatal(decodeErr)
			}
		}
		return result, e
	}
	for _, scopes := range [][]string{nil, {"cortex.manage"}, {"cortex.run"}, {"cortex.optimization.read"}, {"cortex.optimization.run"}, {"cortex.optimization.activate"}} {
		denied := p
		denied.User = &dashauth.UserInfo{Subject: "fixture", Scopes: scopes}
		if _, err = invoke(d, denied, "review"); !errors.Is(err, dc.ErrPermissionDenied) {
			t.Fatalf("permission %v: %v", scopes, err)
		}
	}
	if _, err = invoke(d, p, ""); !errors.Is(err, dc.ErrBadRequest) {
		t.Fatalf("payload key admitted: %v", err)
	}
	noAudit, _, _, _ := optimizationCommandDispatcher(t, service, cortex.ScopeFromContext(ctx), nil)
	if _, err = invoke(noAudit, p, "review"); !errors.Is(err, dc.ErrUnavailable) {
		t.Fatalf("missing audit: %v", err)
	}
	unavailable, _, _, _ := optimizationCommandDispatcher(t, nil, cortex.ScopeFromContext(ctx), audit)
	if _, err = invoke(unavailable, p, "review"); !errors.Is(err, dc.ErrUnavailable) {
		t.Fatalf("missing optimizer: %v", err)
	}
	foreign := p
	foreign.Claims = map[string]any{"cortex_scope": cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "foreign"}}}}
	if _, err = invoke(d, foreign, "review"); !errors.Is(err, dc.ErrNotFound) {
		t.Fatalf("foreign review: %v", err)
	}
	input["expected_revision"] = current.Revision - 1
	if _, err = invoke(d, p, "review"); !errors.Is(err, dc.ErrConflict) {
		t.Fatalf("stale review: %v", err)
	}
	input["expected_revision"] = current.Revision
	auditFailure = true
	_, err = invoke(d, p, "review")
	var failure *dc.Error
	if !errors.As(err, &failure) || failure.Code != dc.CodeUnavailable || failure.Details["command_attempted"] != true {
		t.Fatalf("lost audit outcome: %v", err)
	}
	auditFailure = false
	recovered, err := invoke(d, p, "review")
	if err != nil || recovered.State != opt.StateCompleted || recovered.Revision != current.Revision+1 {
		t.Fatalf("review recovery: %+v %v", recovered, err)
	}
	auth.denied = true
	if _, err = invoke(d, p, "review"); !errors.Is(err, dc.ErrPermissionDenied) {
		t.Fatalf("current host denial: %v", err)
	}
	auth.denied = false
	input["reason"] = "different reason"
	if _, err = invoke(d, p, "review"); !errors.Is(err, dc.ErrConflict) {
		t.Fatalf("changed review: %v", err)
	}
	input["reason"] = "Keep current baseline"
	invalidates := []string{"optimization.experiments.list", "optimization.experiments.detail", "optimization.comparison", "optimization.events.list"}
	intent, ok := registry.Intent(ContributorName, "optimization.decisions.retain", 1)
	if !ok || !reflect.DeepEqual(intent.Invalidates, invalidates) {
		t.Fatalf("retain invalidations: %+v", intent)
	}
	receipt := optimizationHTTPReceipt[opt.Experiment](t, d, registry, wardens, p, "optimization.decisions.retain", "review", input, invalidates)
	if !reflect.DeepEqual(receipt, recovered) {
		t.Fatal("HTTP retry changed review")
	}
	if cache.calls != 0 {
		t.Fatalf("review used generic replay cache: %d", cache.calls)
	}
	if _, err = repo.GetActivation(ctx, change.AgentID, ""); !errors.Is(err, opt.ErrNotFound) {
		t.Fatalf("review changed route: %v", err)
	}
}
