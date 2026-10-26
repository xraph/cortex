package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	dashauth "github.com/xraph/forge/extensions/dashboard/auth"
	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/transport"
	"github.com/xraph/forge/extensions/dashboard/security"

	opt "github.com/xraph/cortex/optimization"
)

func TestOptimizationCapabilityGrants(t *testing.T) {
	service, authority, bundle, principal := optimizationContractFixture(t)
	d, registry, wardens, _ := optimizationCommandDispatcher(t, service, bundle.Experiment.Spec.Scope, nil)
	payload, err := json.Marshal(map[string]any{"experiment_id": bundle.Experiment.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	handler := transport.NewHandlerWithCSRF(registry, wardens, d, dc.NoopAuditEmitter{}, security.NewCSRFManager())
	request := dc.Request{Envelope: "v1", Contributor: ContributorName, Intent: "optimization.capabilities", IntentVersion: 1, Kind: dc.KindQuery, Payload: payload}
	for _, permission := range []string{"", "manage", "run", "activate", "artifacts"} {
		t.Run(permission, func(t *testing.T) {
			principal.User = &dashauth.UserInfo{Subject: "fixture", Scopes: []string{"cortex.optimization.read", "cortex.optimization." + permission}}
			body, e := json.Marshal(request)
			if e != nil {
				t.Fatal(e)
			}
			httpRequest := httptest.NewRequestWithContext(dashauth.WithUser(context.Background(), principal.User), http.MethodPost, "/api/dashboard/v1", bytes.NewReader(body))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httpRequest)
			if response.Code != http.StatusOK {
				t.Fatalf("capability HTTP: %d %s", response.Code, response.Body)
			}
			var receipt struct {
				Data opt.CapabilitySnapshot `json:"data"`
			}
			if e = json.Unmarshal(response.Body.Bytes(), &receipt); e != nil {
				t.Fatal(e)
			}
			result := receipt.Data
			if result.TrialsReady || result.Checks[opt.CapabilityEvaluation] != opt.ReadinessUnavailable {
				t.Fatalf("missing provider became ready: %+v", result)
			}
			for action, allowed := range result.Permissions {
				want := action == opt.ActionRead
				switch permission {
				case "manage":
					want = want || action == opt.ActionCreate || action == opt.ActionRetain || action == opt.ActionRetryPublication || action == opt.ActionRevokeLesson
				case "run":
					want = want || action == opt.ActionStart || action == opt.ActionCancel || action == opt.ActionResume
				case "activate":
					want = want || action == opt.ActionActivate || action == opt.ActionRollback || action == opt.ActionDeactivate
				case "artifacts":
					want = want || action == opt.ActionReadArtifact
				}
				if allowed != want {
					t.Fatalf("permission %q action %q: %v want %v", permission, action, allowed, want)
				}
			}
		})
	}
	authority.denied = true
	if _, _, err = d.Dispatch(context.Background(), request, principal); !errors.Is(err, dc.ErrPermissionDenied) {
		t.Fatalf("cached host permission: %v", err)
	}
	authority.denied = false
	for _, scope := range []string{"cortex.manage", "cortex.run", "cortex.optimization.manage", "cortex.optimization.activate"} {
		principal.User = &dashauth.UserInfo{Subject: "fixture", Scopes: []string{scope}}
		if _, _, err = d.Dispatch(context.Background(), request, principal); !errors.Is(err, dc.ErrPermissionDenied) {
			t.Fatalf("missing independent read grant %s: %v", scope, err)
		}
	}
}
