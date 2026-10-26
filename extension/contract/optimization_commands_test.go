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

	dashauth "github.com/xraph/forge/extensions/dashboard/auth"
	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"
	"github.com/xraph/forge/extensions/dashboard/contract/transport"
	"github.com/xraph/forge/extensions/dashboard/security"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/operator"
	"github.com/xraph/cortex/store/scopespy"
)

type optimizationCacheTrap struct{ calls int }

func (c *optimizationCacheTrap) Lookup(context.Context, string, string) (*dispatcher.IdempotencyCached, bool) {
	c.calls++
	return nil, false
}
func (c *optimizationCacheTrap) Store(context.Context, string, string, dispatcher.IdempotencyCached) error {
	c.calls++
	return opt.ErrUnavailable
}

func optimizationCommandDispatcher(t *testing.T, service *opt.Optimizer, scope cortex.Scope, audit func(context.Context, AuditEvent) error) (*dispatcher.Dispatcher, dc.Registry, dc.WardenRegistry, *optimizationCacheTrap) {
	t.Helper()
	runtime, err := engine.New(engine.WithStore(scopespy.New()))
	if err != nil {
		t.Fatal(err)
	}
	cache := &optimizationCacheTrap{}
	d := dispatcher.NewWithOptions(nil, dispatcher.WithIdempotencyStore(cache))
	registry, wardens := dc.NewRegistry(), dc.NewWardenRegistry()
	if err = Register(d, registry, wardens, Deps{Engine: runtime, Optimization: service, DefaultScope: scope, Audit: audit}); err != nil {
		t.Fatal(err)
	}
	return d, registry, wardens, cache
}

func lifecycleDispatch(t *testing.T, d *dispatcher.Dispatcher, p dc.Principal, action, key string, input operator.LifecycleInput) (json.RawMessage, error) {
	t.Helper()
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := d.Dispatch(context.Background(), dc.Request{Contributor: ContributorName, Intent: "optimization.experiments." + action, IntentVersion: 1, Kind: dc.KindCommand, IdempotencyKey: key, Payload: payload}, p)
	return data, err
}

func TestOptimizationLifecycleContractAdmission(t *testing.T) {
	for _, action := range []string{"start", "cancel", "resume"} {
		t.Run(action, func(t *testing.T) {
			service, auth, bundle, p := optimizationContractFixture(t)
			p.User.Scopes = []string{"cortex.optimization.run"}
			ctx := cortex.WithPrincipal(cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope), p)
			current, err := service.Get(ctx, bundle.Experiment.ID)
			if err != nil {
				t.Fatal(err)
			}
			if action == "resume" {
				if _, err = service.Start(ctx, current.ID, current.Revision); err != nil {
					t.Fatal(err)
				}
				if err = service.WorkOnce(ctx, current.ID); !errors.Is(err, opt.ErrUnavailable) {
					t.Fatal(err)
				}
				current, err = service.Get(ctx, current.ID)
				if err != nil {
					t.Fatal(err)
				}
				current, err = service.Pause(ctx, current.ID, current.Revision)
				if err != nil {
					t.Fatal(err)
				}
			}
			d, _, _, cache := optimizationCommandDispatcher(t, service, bundle.Experiment.Spec.Scope, func(context.Context, AuditEvent) error { return nil })
			input := operator.LifecycleInput{ID: current.ID.String(), ExpectedRevision: current.Revision}
			for _, scopes := range [][]string{nil, {"cortex.run"}, {"cortex.manage"}, {"cortex.optimization.read"}, {"cortex.optimization.manage"}, {"cortex.optimization.activate"}} {
				denied := p
				denied.User = &dashauth.UserInfo{Subject: "fixture", Scopes: scopes}
				if _, err = lifecycleDispatch(t, d, denied, action, "key", input); !errors.Is(err, dc.ErrPermissionDenied) {
					t.Fatalf("permission %v: %v", scopes, err)
				}
			}
			if _, err = lifecycleDispatch(t, d, p, action, "", input); !errors.Is(err, dc.ErrBadRequest) {
				t.Fatalf("missing key: %v", err)
			}
			missingAudit, _, _, _ := optimizationCommandDispatcher(t, service, bundle.Experiment.Spec.Scope, nil)
			if _, err = lifecycleDispatch(t, missingAudit, p, action, "key", input); !errors.Is(err, dc.ErrUnavailable) {
				t.Fatalf("missing audit: %v", err)
			}
			unavailable, _, _, _ := optimizationCommandDispatcher(t, nil, bundle.Experiment.Spec.Scope, func(context.Context, AuditEvent) error { return nil })
			if _, err = lifecycleDispatch(t, unavailable, p, action, "key", input); !errors.Is(err, dc.ErrUnavailable) {
				t.Fatalf("missing optimizer: %v", err)
			}
			foreign := p
			foreign.Claims = map[string]any{"cortex_scope": cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "foreign"}}}}
			if _, err = lifecycleDispatch(t, d, foreign, action, "key", input); !errors.Is(err, dc.ErrNotFound) {
				t.Fatalf("foreign resource: %v", err)
			}
			auth.denied = true
			if _, err = lifecycleDispatch(t, d, p, action, "key", input); !errors.Is(err, dc.ErrPermissionDenied) {
				t.Fatalf("host denial: %v", err)
			}
			auth.denied = false
			data, err := lifecycleDispatch(t, d, p, action, "key", input)
			if err != nil {
				t.Fatal(err)
			}
			var changed opt.Experiment
			if err = json.Unmarshal(data, &changed); err != nil {
				t.Fatal(err)
			}
			if changed.Revision != current.Revision+1 {
				t.Fatalf("transition: %+v", changed)
			}
			if _, err = lifecycleDispatch(t, d, p, action, "key", input); err != nil {
				t.Fatalf("retry: %v", err)
			}
			auth.denied = true
			if _, err = lifecycleDispatch(t, d, p, action, "key", input); !errors.Is(err, dc.ErrPermissionDenied) {
				t.Fatalf("retry bypassed current grant: %v", err)
			}
			auth.denied = false
			if _, err = lifecycleDispatch(t, d, p, action, "fresh-key", input); !errors.Is(err, dc.ErrConflict) {
				t.Fatalf("stale revision: %v", err)
			}
			altered := input
			altered.ExpectedRevision++
			if _, err = lifecycleDispatch(t, d, p, action, "key", altered); !errors.Is(err, dc.ErrConflict) {
				t.Fatalf("key collision: %v", err)
			}
			if cache.calls != 0 {
				t.Fatalf("domain receipt used generic replay cache: %d", cache.calls)
			}
		})
	}
}

func TestOptimizationLifecycleContractAuditRecovery(t *testing.T) {
	service, _, bundle, p := optimizationContractFixture(t)
	p.User.Scopes = []string{"cortex.optimization.run"}
	ctx := cortex.WithPrincipal(cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope), p)
	failOutcome := true
	d, _, _, _ := optimizationCommandDispatcher(t, service, bundle.Experiment.Spec.Scope, func(_ context.Context, event AuditEvent) error {
		if failOutcome && event.Outcome == "succeeded" {
			return errors.New("audit unavailable")
		}
		return nil
	})
	input := operator.LifecycleInput{ID: bundle.Experiment.ID.String(), ExpectedRevision: 1}
	_, err := lifecycleDispatch(t, d, p, "start", "lost-response", input)
	var failure *dc.Error
	if !errors.As(err, &failure) || failure.Code != dc.CodeUnavailable || failure.Details["command_attempted"] != true {
		t.Fatalf("lost audit outcome: %v", err)
	}
	current, err := service.Get(ctx, bundle.Experiment.ID)
	if err != nil || current.State != opt.StateQueued {
		t.Fatalf("saved command: %+v %v", current, err)
	}
	failOutcome = false
	data, err := lifecycleDispatch(t, d, p, "start", "lost-response", input)
	if err != nil {
		t.Fatal(err)
	}
	var recovered opt.Experiment
	if err = json.Unmarshal(data, &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.Revision != current.Revision {
		t.Fatal("audit retry repeated domain transition")
	}
	if _, err = lifecycleDispatch(t, d, p, "cancel", "lost-response", input); !errors.Is(err, dc.ErrConflict) {
		t.Fatalf("action collision: %v", err)
	}
}

func TestOptimizationLifecycleHTTPEnvelopeInvalidation(t *testing.T) {
	for _, action := range []string{"start", "cancel", "resume"} {
		t.Run(action, func(t *testing.T) {
			service, _, bundle, p := optimizationContractFixture(t)
			p.User.Scopes = []string{"cortex.optimization.run"}
			ctx := cortex.WithPrincipal(cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope), p)
			current, err := service.Get(ctx, bundle.Experiment.ID)
			if err != nil {
				t.Fatal(err)
			}
			if action == "resume" {
				if _, err = service.Start(ctx, current.ID, current.Revision); err != nil {
					t.Fatal(err)
				}
				if err = service.WorkOnce(ctx, current.ID); !errors.Is(err, opt.ErrUnavailable) {
					t.Fatal(err)
				}
				current, err = service.Get(ctx, current.ID)
				if err != nil {
					t.Fatal(err)
				}
				current, err = service.Pause(ctx, current.ID, current.Revision)
				if err != nil {
					t.Fatal(err)
				}
			}
			d, registry, wardens, _ := optimizationCommandDispatcher(t, service, bundle.Experiment.Spec.Scope, func(context.Context, AuditEvent) error { return nil })
			csrf := security.NewCSRFManager()
			handler := transport.NewHandlerWithCSRF(registry, wardens, d, dc.NoopAuditEmitter{}, csrf)
			payload, err := json.Marshal(operator.LifecycleInput{ID: current.ID.String(), ExpectedRevision: current.Revision})
			if err != nil {
				t.Fatal(err)
			}
			envelope := dc.Request{Envelope: "v1", Contributor: ContributorName, Intent: "optimization.experiments." + action, IntentVersion: 1, Kind: dc.KindCommand, IdempotencyKey: "envelope-key", CSRF: csrf.GenerateToken(), Payload: payload}
			for _, invalid := range []struct {
				name   string
				mutate func(*dc.Request)
				status int
			}{
				{"missing key", func(r *dc.Request) { r.IdempotencyKey = "" }, http.StatusBadRequest},
				{"invalid csrf", func(r *dc.Request) { r.CSRF = "invalid" }, http.StatusForbidden},
				{"wrong kind", func(r *dc.Request) { r.Kind = dc.KindQuery }, http.StatusBadRequest},
			} {
				t.Run(invalid.name, func(t *testing.T) {
					rejected := envelope
					invalid.mutate(&rejected)
					body, encodeErr := json.Marshal(rejected)
					if encodeErr != nil {
						t.Fatal(encodeErr)
					}
					request := httptest.NewRequestWithContext(dashauth.WithUser(context.Background(), p.User), http.MethodPost, "/api/dashboard/v1", bytes.NewReader(body))
					request.Header.Set("Idempotency-Key", "header-cannot-supply-envelope")
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, request)
					if response.Code != invalid.status {
						t.Fatalf("rejected envelope: %d %s", response.Code, response.Body)
					}
					unchanged, readErr := service.Get(ctx, current.ID)
					if readErr != nil || unchanged.Revision != current.Revision {
						t.Fatalf("rejected command changed state: %+v %v", unchanged, readErr)
					}
				})
			}
			for range 2 {
				body, encodeErr := json.Marshal(envelope)
				if encodeErr != nil {
					t.Fatal(encodeErr)
				}
				request := httptest.NewRequestWithContext(dashauth.WithUser(context.Background(), p.User), http.MethodPost, "/api/dashboard/v1", bytes.NewReader(body))
				request.Header.Set("Idempotency-Key", "ignored-header")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != 200 {
					t.Fatalf("HTTP envelope: %d %s", response.Code, response.Body)
				}
				var result struct {
					Data opt.Experiment  `json:"data"`
					Meta dc.ResponseMeta `json:"meta"`
				}
				if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				expected := []string{"optimization.experiments.list", "optimization.experiments.detail", "optimization.trials.list", "optimization.trials.detail", "optimization.comparison", "optimization.events.list"}
				if !reflect.DeepEqual(result.Meta.Invalidates, expected) {
					t.Fatalf("invalidates: %v", result.Meta.Invalidates)
				}
				if result.Data.Revision != current.Revision+1 {
					t.Fatalf("HTTP retry changed revision: %+v", result.Data)
				}
			}
		})
	}
}
