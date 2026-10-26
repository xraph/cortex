package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func optimizationPOST(ctx context.Context, t *testing.T, handler http.Handler, path string, keys []string, input any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/optimization"+path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header["Idempotency-Key"] = keys
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestOptimizationLifecycleHTTPAdmission(t *testing.T) {
	for _, action := range []string{"start", "cancel", "resume"} {
		t.Run(action, func(t *testing.T) {
			service, auth, bundle, ctx := optimizationHTTPFixture(t)
			current, err := service.Get(ctx, bundle.Experiment.ID)
			if err != nil {
				t.Fatal(err)
			}
			if action == "resume" {
				if _, err = service.Start(ctx, current.ID, current.Revision); err != nil {
					t.Fatal(err)
				}
				if err = service.WorkOnce(ctx, current.ID); !errors.Is(err, opt.ErrUnavailable) {
					t.Fatalf("fixture worker: %v", err)
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
			handler := New(nil, nil, WithOptimization(service)).Handler()
			path := "/experiments/" + current.ID.String() + "/" + action
			payload := map[string]any{"expected_revision": current.Revision}
			for _, keys := range [][]string{nil, {""}, {"duplicate", "duplicate"}, {" padded"}} {
				response := optimizationPOST(ctx, t, handler, path, keys, payload)
				if response.Code != 400 {
					t.Fatalf("invalid key: %d %s", response.Code, response.Body)
				}
			}
			auth.denied = true
			denied := optimizationPOST(ctx, t, handler, path, []string{"key"}, payload)
			auth.denied = false
			if denied.Code != 403 {
				t.Fatalf("denied: %d %s", denied.Code, denied.Body)
			}
			noScope := optimizationPOST(context.Background(), t, handler, path, []string{"key"}, payload)
			if noScope.Code != 403 {
				t.Fatalf("no scope: %d %s", noScope.Code, noScope.Body)
			}
			foreign := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
			hidden := optimizationPOST(foreign, t, handler, path, []string{"key"}, payload)
			if hidden.Code != 404 {
				t.Fatalf("foreign: %d %s", hidden.Code, hidden.Body)
			}
			unavailable := optimizationPOST(ctx, t, New(nil, nil).Handler(), path, []string{"key"}, payload)
			if unavailable.Code != 503 {
				t.Fatalf("unconfigured: %d %s", unavailable.Code, unavailable.Body)
			}
			invalid := optimizationPOST(ctx, t, handler, path, []string{"key"}, map[string]any{"expected_revision": -1})
			if invalid.Code != 400 {
				t.Fatalf("revision: %d %s", invalid.Code, invalid.Body)
			}
			response := optimizationPOST(ctx, t, handler, path, []string{"key"}, payload)
			if response.Code != 200 {
				t.Fatalf("command: %d %s", response.Code, response.Body)
			}
			var result opt.Experiment
			if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.ID != current.ID || result.Revision != current.Revision+1 {
				t.Fatalf("transition: %+v", result)
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("command response can be cached")
			}
			retry := optimizationPOST(ctx, t, handler, path, []string{"key"}, payload)
			if retry.Code != 200 {
				t.Fatalf("retry: %d %s", retry.Code, retry.Body)
			}
			var again opt.Experiment
			if err = json.Unmarshal(retry.Body.Bytes(), &again); err != nil {
				t.Fatal(err)
			}
			if again.Revision != result.Revision {
				t.Fatalf("retry appended transition: %+v", again)
			}
			auth.denied = true
			revoked := optimizationPOST(ctx, t, handler, path, []string{"key"}, payload)
			auth.denied = false
			if revoked.Code != 403 {
				t.Fatalf("cached grant: %d %s", revoked.Code, revoked.Body)
			}
			stale := optimizationPOST(ctx, t, handler, path, []string{"new-key"}, payload)
			if stale.Code != 409 {
				t.Fatalf("stale revision: %d %s", stale.Code, stale.Body)
			}
		})
	}
}

func TestOptimizationLifecycleHTTPBindsHeaderPathAndAction(t *testing.T) {
	service, _, bundle, ctx := optimizationHTTPFixture(t)
	other := storetest.Fixture(t, "http")
	if _, err := service.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	handler := New(nil, nil, WithOptimization(service)).Handler()
	path := "/experiments/" + bundle.Experiment.ID.String()
	payload := map[string]any{"id": other.Experiment.ID.String(), "expected_revision": 1, "idempotency_key": "body-key"}
	missing := optimizationPOST(ctx, t, handler, path+"/start", nil, payload)
	if missing.Code != 400 {
		t.Fatalf("payload supplied key: %d %s", missing.Code, missing.Body)
	}
	response := optimizationPOST(ctx, t, handler, path+"/start", []string{"header-key"}, payload)
	if response.Code != 200 {
		t.Fatalf("start: %d %s", response.Code, response.Body)
	}
	var result opt.Experiment
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ID != bundle.Experiment.ID || result.State != opt.StateQueued {
		t.Fatalf("body redirected path: %+v", result)
	}
	untouched, err := service.Get(ctx, other.Experiment.ID)
	if err != nil || untouched.State != opt.StateDraft {
		t.Fatalf("other mutated: %+v %v", untouched, err)
	}
	for _, action := range []string{"resume", "cancel"} {
		conflict := optimizationPOST(ctx, t, handler, path+"/"+action, []string{"header-key"}, payload)
		if conflict.Code != 409 {
			t.Fatalf("action collision %s: %d %s", action, conflict.Code, conflict.Body)
		}
	}
	cancelled := optimizationPOST(ctx, t, handler, path+"/cancel", []string{"cancel"}, map[string]any{"expected_revision": result.Revision})
	if cancelled.Code != 200 {
		t.Fatalf("cancel: %d %s", cancelled.Code, cancelled.Body)
	}
	retry := optimizationPOST(ctx, t, handler, path+"/start", []string{"header-key"}, payload)
	if err = json.Unmarshal(retry.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if retry.Code != 200 || result.State != opt.StateCancelled {
		t.Fatalf("old start revived work: %d %s", retry.Code, retry.Body)
	}
}

func TestOptimizationLifecycleHTTPReceiptSurvivesRestart(t *testing.T) {
	service, auth, bundle, ctx, restart := optimizationHTTPRecoveryFixture(t)
	path := "/experiments/" + bundle.Experiment.ID.String()
	start := map[string]any{"expected_revision": 1}
	handler := New(nil, nil, WithOptimization(service)).Handler()
	response := optimizationPOST(ctx, t, handler, path+"/start", []string{"lost-start"}, start)
	if response.Code != http.StatusOK {
		t.Fatalf("start: %d %s", response.Code, response.Body)
	}
	service = restart()
	handler = New(nil, nil, WithOptimization(service)).Handler()
	auth.denied = true
	response = optimizationPOST(ctx, t, handler, path+"/start", []string{"lost-start"}, start)
	if response.Code != http.StatusForbidden {
		t.Fatalf("recovered revoked grant: %d %s", response.Code, response.Body)
	}
	auth.denied = false
	response = optimizationPOST(ctx, t, handler, path+"/start", []string{"lost-start"}, start)
	var queued opt.Experiment
	if err := json.Unmarshal(response.Body.Bytes(), &queued); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || queued.Revision != 2 || queued.State != opt.StateQueued {
		t.Fatalf("recovered start: %d %s", response.Code, response.Body)
	}
	cancel := map[string]any{"expected_revision": queued.Revision}
	response = optimizationPOST(ctx, t, handler, path+"/cancel", []string{"lost-cancel"}, cancel)
	if response.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", response.Code, response.Body)
	}
	service = restart()
	handler = New(nil, nil, WithOptimization(service)).Handler()
	for _, retry := range []struct {
		action, key string
		payload     any
	}{
		{"cancel", "lost-cancel", cancel},
		{"start", "lost-start", start},
	} {
		response = optimizationPOST(ctx, t, handler, path+"/"+retry.action, []string{retry.key}, retry.payload)
		var current opt.Experiment
		if err := json.Unmarshal(response.Body.Bytes(), &current); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || current.State != opt.StateCancelled || current.Revision != 3 {
			t.Fatalf("recovered %s repeated mutation: %d %s", retry.action, response.Code, response.Body)
		}
	}
}
