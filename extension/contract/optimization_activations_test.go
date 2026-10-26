package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	dashauth "github.com/xraph/forge/extensions/dashboard/auth"
	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"
	"github.com/xraph/forge/extensions/dashboard/contract/transport"
	"github.com/xraph/forge/extensions/dashboard/security"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/operator"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/storetest"
)

type optimizationContractCompatibility struct{ stale bool }

func (c *optimizationContractCompatibility) Check(_ context.Context, v opt.HarnessVersion) (opt.CompatibilityReport, error) {
	return opt.CompatibilityReport{Compatible: !c.stale, Version: opt.VersionRef{ID: v.ID, Digest: v.Digest}, PolicyDigest: v.PolicyDigest}, nil
}

type optimizationContractArtifacts struct{}

func (optimizationContractArtifacts) Get(context.Context, opt.ArtifactRef) (opt.Artifact, error) {
	return opt.Artifact{}, opt.ErrUnavailable
}

func TestOptimizationActivationContractAdmission(t *testing.T) {
	repo, ctx, change := storetest.ReviewedActivationFixture(t, func(t *testing.T, now func() time.Time) store.Store {
		r, err := memory.New(memory.WithClock(now))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.Close() })
		return r
	})
	p := dc.Principal{User: &dashauth.UserInfo{Subject: "fixture", Scopes: []string{"cortex.optimization.activate"}}, Claims: map[string]any{"cortex_scope": cortex.ScopeFromContext(ctx)}}
	auth := &optimizationTestAuthority{}
	compatibility := &optimizationContractCompatibility{}
	verifier, err := opt.NewArtifactVerifier(optimizationContractArtifacts{}, 1<<20, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	service, err := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Caller: auth, Authorizer: auth, IdentityResolver: auth, Handler: auth, Worker: opt.DefaultWorkerConfig(), Compatibility: compatibility, ArtifactVerifier: verifier})
	if err != nil {
		t.Fatal(err)
	}
	auditFailure := false
	audit := func(_ context.Context, event AuditEvent) error {
		if auditFailure && event.Outcome == "succeeded" {
			return errors.New("audit unavailable")
		}
		return nil
	}
	d, registry, wardens, cache := optimizationCommandDispatcher(t, service, cortex.ScopeFromContext(ctx), audit)
	invoke := func(disp *dispatcher.Dispatcher, principal dc.Principal, action, key string, input any) (*opt.Activation, error) {
		payload, encodeErr := json.Marshal(input)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		data, _, callErr := disp.Dispatch(context.Background(), dc.Request{Contributor: ContributorName, Intent: "optimization.activations." + action, IntentVersion: 1, Kind: dc.KindCommand, IdempotencyKey: key, Payload: payload}, principal)
		if callErr != nil {
			return nil, callErr
		}
		var out opt.Activation
		if decodeErr := json.Unmarshal(data, &out); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		return &out, nil
	}
	input := operator.ActivateInput{ActivationRouteInput: operator.ActivationRouteInput{AgentID: change.AgentID.String(), Reason: "reviewed activation"}, VersionID: change.Version.ID.String(), VersionDigest: change.Version.Digest, DecisionID: change.DecisionID.String()}
	for _, action := range []string{"activate", "rollback", "deactivate"} {
		for _, scopes := range [][]string{nil, {"cortex.manage"}, {"cortex.run"}, {"cortex.optimization.read"}, {"cortex.optimization.manage"}, {"cortex.optimization.run"}} {
			denied := p
			denied.User = &dashauth.UserInfo{Subject: "fixture", Scopes: scopes}
			if _, err = invoke(d, denied, action, "key", input); !errors.Is(err, dc.ErrPermissionDenied) {
				t.Fatalf("%s permission %v: %v", action, scopes, err)
			}
		}
		if _, err = invoke(d, p, action, "", input); !errors.Is(err, dc.ErrBadRequest) {
			t.Fatalf("%s missing key: %v", action, err)
		}
		missingAudit, _, _, _ := optimizationCommandDispatcher(t, service, cortex.ScopeFromContext(ctx), nil)
		if _, err = invoke(missingAudit, p, action, "key", input); !errors.Is(err, dc.ErrUnavailable) {
			t.Fatalf("%s missing audit: %v", action, err)
		}
		unavailable, _, _, _ := optimizationCommandDispatcher(t, nil, cortex.ScopeFromContext(ctx), audit)
		if _, err = invoke(unavailable, p, action, "key", input); !errors.Is(err, dc.ErrUnavailable) {
			t.Fatalf("%s missing optimizer: %v", action, err)
		}
	}
	auth.denied = true
	if _, err = invoke(d, p, "activate", "activate", input); !errors.Is(err, dc.ErrPermissionDenied) {
		t.Fatalf("host denial: %v", err)
	}
	auth.denied = false
	compatibility.stale = true
	if _, err = invoke(d, p, "activate", "activate", input); !errors.Is(err, dc.ErrConflict) {
		t.Fatalf("stale source: %v", err)
	}
	compatibility.stale = false
	foreign := p
	foreign.Claims = map[string]any{"cortex_scope": cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "foreign"}}}}
	if _, err = invoke(d, foreign, "activate", "activate", input); !errors.Is(err, dc.ErrNotFound) {
		t.Fatalf("foreign evidence: %v", err)
	}
	auditFailure = true
	_, err = invoke(d, p, "activate", "activate", input)
	var failure *dc.Error
	if !errors.As(err, &failure) || failure.Code != dc.CodeUnavailable || failure.Details["command_attempted"] != true {
		t.Fatalf("lost audit outcome: %v", err)
	}
	auditFailure = false
	active, err := invoke(d, p, "activate", "activate", input)
	if err != nil || active.Generation != 1 {
		t.Fatalf("activation audit recovery: %+v %v", active, err)
	}
	auth.denied = true
	if _, err = invoke(d, p, "activate", "activate", input); !errors.Is(err, dc.ErrPermissionDenied) {
		t.Fatalf("retry ignored current authority: %v", err)
	}
	auth.denied = false
	rollback := operator.RollbackInput{ActivationRouteInput: input.ActivationRouteInput, TargetID: active.ID.String()}
	rollback.ExpectedGeneration = 1
	if _, err = invoke(d, p, "rollback", "activate", rollback); !errors.Is(err, dc.ErrConflict) {
		t.Fatalf("action collision: %v", err)
	}
	rolled, err := invoke(d, p, "rollback", "rollback", rollback)
	if err != nil || rolled.Generation != 2 {
		t.Fatalf("rollback: %+v %v", rolled, err)
	}
	disable := input.ActivationRouteInput
	disable.ExpectedGeneration = 2
	disabled, err := invoke(d, p, "deactivate", "deactivate", disable)
	if err != nil || disabled.Generation != 3 || !disabled.Deactivated {
		t.Fatalf("deactivate: %+v %v", disabled, err)
	}
	if _, err = invoke(d, p, "activate", "activate", input); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.GetActivation(ctx, change.AgentID, ""); !errors.Is(err, opt.ErrInactiveActivation) {
		t.Fatalf("old retry enabled route: %v", err)
	}
	for _, check := range []struct {
		action, key string
		input       any
		generation  int64
	}{
		{"activate", "activate", input, 1},
		{"rollback", "rollback", rollback, 2},
		{"deactivate", "deactivate", disable, 3},
	} {
		receipt := activationHTTPReceipt(t, d, registry, wardens, p, check.action, check.key, check.input)
		if receipt.Generation != check.generation {
			t.Fatalf("HTTP retry changed generation: %+v", receipt)
		}
	}
	if _, err = repo.GetActivation(ctx, change.AgentID, ""); !errors.Is(err, opt.ErrInactiveActivation) {
		t.Fatalf("HTTP replay changed current route: %v", err)
	}
	if cache.calls != 0 {
		t.Fatalf("activation used generic replay cache: %d", cache.calls)
	}
}

func TestOptimizationActivationInvalidations(t *testing.T) {
	_, _, bundle, _ := optimizationContractFixture(t)
	_, registry, _, _ := optimizationCommandDispatcher(t, nil, bundle.Experiment.Spec.Scope, nil)
	expected := []string{"optimization.activations.list", "optimization.versions.list", "optimization.versions.detail", "optimization.experiments.detail", "optimization.capabilities", "optimization.events.list"}
	for _, action := range []string{"activate", "rollback", "deactivate"} {
		intent, ok := registry.Intent(ContributorName, "optimization.activations."+action, 1)
		if !ok {
			t.Fatal("activation intent missing")
		}
		if !reflect.DeepEqual(intent.Invalidates, expected) {
			t.Fatalf("%s invalidates: %v", action, intent.Invalidates)
		}
	}
}

func activationHTTPReceipt(t *testing.T, d *dispatcher.Dispatcher, registry dc.Registry, wardens dc.WardenRegistry, p dc.Principal, action, key string, input any) opt.Activation {
	t.Helper()
	csrf := security.NewCSRFManager()
	handler := transport.NewHandlerWithCSRF(registry, wardens, d, dc.NoopAuditEmitter{}, csrf)
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	envelope := dc.Request{Envelope: "v1", Contributor: ContributorName, Intent: "optimization.activations." + action, IntentVersion: 1, Kind: dc.KindCommand, IdempotencyKey: key, CSRF: csrf.GenerateToken(), Payload: payload}
	var receipt opt.Activation
	for _, valid := range []bool{false, true} {
		request := envelope
		if !valid {
			request.CSRF = "invalid"
		}
		body, encodeErr := json.Marshal(request)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		httpRequest := httptest.NewRequestWithContext(dashauth.WithUser(context.Background(), p.User), http.MethodPost, "/api/dashboard/v1", bytes.NewReader(body))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httpRequest)
		if !valid {
			if response.Code != http.StatusForbidden {
				t.Fatalf("%s CSRF: %d %s", action, response.Code, response.Body)
			}
			continue
		}
		if response.Code != http.StatusOK {
			t.Fatalf("%s HTTP: %d %s", action, response.Code, response.Body)
		}
		var result struct {
			Data opt.Activation  `json:"data"`
			Meta dc.ResponseMeta `json:"meta"`
		}
		if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		expected := []string{"optimization.activations.list", "optimization.versions.list", "optimization.versions.detail", "optimization.experiments.detail", "optimization.capabilities", "optimization.events.list"}
		if !reflect.DeepEqual(result.Meta.Invalidates, expected) {
			t.Fatalf("%s HTTP invalidates: %v", action, result.Meta.Invalidates)
		}
		receipt = result.Data
	}
	return receipt
}
