package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dashauth "github.com/xraph/forge/extensions/dashboard/auth"
	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/transport"
	"github.com/xraph/forge/extensions/dashboard/security"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store/memory"
)

func TestOptimizationCatalogContractCurrentAccess(t *testing.T) {
	repo, err := memory.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "catalog-contract"}}}
	sourceErr := error(nil)
	catalog, err := opt.NewApprovedCatalog(opt.ApprovedCatalogConfig{Choices: []opt.CatalogChoice{{Scope: scope, Kind: opt.CatalogEvaluation, Ref: opt.ContentRef{Ref: "mapped-plan", Revision: "1", Digest: strings.Repeat("a", 64)}, Name: "Approved evaluation"}}, Authorize: func(context.Context, opt.CatalogChoice) error { return sourceErr }})
	if err != nil {
		t.Fatal(err)
	}
	auth := &optimizationTestAuthority{}
	service, err := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Caller: auth, Authorizer: auth, IdentityResolver: auth, Handler: auth, Worker: opt.DefaultWorkerConfig(), Catalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	d, registry, wardens, _ := optimizationCommandDispatcher(t, service, scope, nil)
	principal := dc.Principal{User: &dashauth.UserInfo{Subject: "fixture", Scopes: []string{"cortex.optimization.read"}}, Claims: map[string]any{"cortex_scope": scope}}
	for _, permission := range []string{"cortex.manage", "cortex.optimization.manage", "cortex.optimization.run", "cortex.optimization.activate"} {
		denied := principal
		denied.User = &dashauth.UserInfo{Subject: "fixture", Scopes: []string{permission}}
		if _, err = dispatch(t, d, denied, "optimization.catalog", map[string]any{}); !errors.Is(err, dc.ErrPermissionDenied) {
			t.Fatalf("independent read permission %s: %v", permission, err)
		}
	}
	handler := transport.NewHandlerWithCSRF(registry, wardens, d, dc.NoopAuditEmitter{}, security.NewCSRFManager())
	request := dc.Request{Envelope: "v1", Contributor: ContributorName, Intent: "optimization.catalog", IntentVersion: 1, Kind: dc.KindQuery, Payload: json.RawMessage(`{"kind":"evaluation"}`)}
	read := func(status int) opt.ResourcePage[opt.CatalogChoice] {
		body, e := json.Marshal(request)
		if e != nil {
			t.Fatal(e)
		}
		req := httptest.NewRequestWithContext(dashauth.WithUser(context.Background(), principal.User), http.MethodPost, "/api/dashboard/v1", bytes.NewReader(body))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != status {
			t.Fatalf("catalog HTTP: %d want %d %s", response.Code, status, response.Body)
		}
		var receipt struct {
			Data opt.ResourcePage[opt.CatalogChoice] `json:"data"`
		}
		if status == 200 {
			if e = json.Unmarshal(response.Body.Bytes(), &receipt); e != nil {
				t.Fatal(e)
			}
		}
		return receipt.Data
	}
	if page := read(200); len(page.Items) != 1 || !page.Complete {
		t.Fatalf("catalog: %+v", page)
	}
	sourceErr = opt.ErrPolicyDenied
	if page := read(200); len(page.Items) != 0 || !page.Complete {
		t.Fatal("cached source grant")
	}
	sourceErr = opt.ErrUnavailable
	read(503)
	sourceErr = nil
	auth.denied = true
	read(403)
	auth.denied = false
	if _, err = dispatch(t, d, principal, "optimization.catalog", map[string]any{"limit": 101}); !errors.Is(err, dc.ErrBadRequest) {
		t.Fatalf("invalid page: %v", err)
	}
	unavailable, _, _, _ := optimizationCommandDispatcher(t, nil, scope, nil)
	if _, err = dispatch(t, unavailable, principal, "optimization.catalog", map[string]any{}); !errors.Is(err, dc.ErrUnavailable) {
		t.Fatalf("unconfigured: %v", err)
	}
}
