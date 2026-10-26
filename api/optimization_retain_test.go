package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestOptimizationRetainHTTPRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retain.db")
	repo, ctx, change := storetest.ReviewedActivationFixture(t, func(t *testing.T, _ func() time.Time) store.Store { return openOptimizationHTTPStore(t, path) })
	t.Cleanup(func() { _ = repo.Close() })
	decision, err := repo.GetDecision(ctx, change.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := repo.GetExperiment(ctx, decision.ExperimentID)
	if err != nil {
		t.Fatal(err)
	}
	auth := &optimizationAuthority{}
	newHandler := func() http.Handler {
		service, e := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Caller: auth, Authorizer: auth, IdentityResolver: auth, Handler: auth, Worker: opt.DefaultWorkerConfig()})
		if e != nil {
			t.Fatal(e)
		}
		return New(nil, nil, WithOptimization(service)).Handler()
	}
	handler := newHandler()
	input := map[string]any{"id": current.ID.String(), "expected_revision": current.Revision, "decision_id": decision.ID.String(), "reason": "Keep the current baseline", "idempotency_key": "forged"}
	call := func(c context.Context, key string, code int) opt.Experiment {
		response := optimizationPOST(c, t, handler, "/decisions/retain", []string{key}, input)
		if response.Code != code {
			t.Fatalf("retain: %d want %d: %s", response.Code, code, response.Body)
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("review response can be cached")
		}
		var result opt.Experiment
		if code == 200 {
			if e := json.Unmarshal(response.Body.Bytes(), &result); e != nil {
				t.Fatal(e)
			}
		}
		return result
	}
	call(ctx, "", 400)
	call(context.Background(), "review", 403)
	foreign := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "foreign"}}})
	call(foreign, "review", 404)
	auth.denied = true
	call(ctx, "review", 403)
	auth.denied = false
	input["expected_revision"] = current.Revision - 1
	call(ctx, "review", 409)
	input["expected_revision"] = current.Revision
	input["decision_id"] = current.ID.String()
	call(ctx, "review", 400)
	input["decision_id"] = decision.ID.String()
	retained := call(ctx, "review", 200)
	if retained.State != opt.StateCompleted || retained.Revision != current.Revision+1 {
		t.Fatalf("review: %+v", retained)
	}
	if err = repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo = openOptimizationHTTPStore(t, path)
	handler = newHandler()
	recovered := call(ctx, "review", 200)
	if !reflect.DeepEqual(retained, recovered) {
		t.Fatal("reopen changed review receipt")
	}
	auth.denied = true
	call(ctx, "review", 403)
	auth.denied = false
	input["reason"] = "different review"
	call(ctx, "review", 409)
	input["reason"] = "Keep the current baseline"
	response := optimizationGET(ctx, handler, "/experiments/"+current.ID.String()+"/comparison")
	if response.Code != 200 {
		t.Fatalf("comparison: %d %s", response.Code, response.Body)
	}
	var comparison struct {
		Decision         *opt.Decision `json:"decision"`
		RetainedBaseline struct {
			Reason     string `json:"reason"`
			DecisionID string `json:"decision_id"`
		} `json:"retained_baseline"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &comparison); err != nil {
		t.Fatal(err)
	}
	if comparison.Decision == nil || !reflect.DeepEqual(*comparison.Decision, decision) || comparison.RetainedBaseline.Reason != input["reason"] || comparison.RetainedBaseline.DecisionID != decision.ID.String() {
		t.Fatalf("comparison lost original evidence or review: %+v", comparison)
	}
	if _, err = repo.GetActivation(ctx, change.AgentID, ""); !errors.Is(err, opt.ErrNotFound) {
		t.Fatalf("retaining changed route: %v", err)
	}
	events, err := repo.ListEvents(ctx, current.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events.Items {
		if event.Type == "baseline_retained" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("review count: %d", count)
	}
}
