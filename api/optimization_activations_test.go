package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/operator"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/storetest"
)

type optimizationCompatibility struct{ stale bool }

func (c *optimizationCompatibility) Check(_ context.Context, v opt.HarnessVersion) (opt.CompatibilityReport, error) {
	return opt.CompatibilityReport{Compatible: !c.stale, Version: opt.VersionRef{ID: v.ID, Digest: v.Digest}, PolicyDigest: v.PolicyDigest}, nil
}

type optimizationArtifacts struct{}

func (optimizationArtifacts) Get(context.Context, opt.ArtifactRef) (opt.Artifact, error) {
	return opt.Artifact{}, opt.ErrUnavailable
}

func TestOptimizationActivationHTTPRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activation.db")
	repo, ctx, change := storetest.ReviewedActivationFixture(t, func(t *testing.T, _ func() time.Time) store.Store { return openOptimizationHTTPStore(t, path) })
	t.Cleanup(func() { _ = repo.Close() })
	auth := &optimizationAuthority{}
	compatibility := &optimizationCompatibility{}
	verifier, err := opt.NewArtifactVerifier(optimizationArtifacts{}, 1<<20, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	newHandler := func() http.Handler {
		service, createErr := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Caller: auth, Authorizer: auth, IdentityResolver: auth, Handler: auth, Worker: opt.DefaultWorkerConfig(), Compatibility: compatibility, ArtifactVerifier: verifier})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return New(nil, nil, WithOptimization(service)).Handler()
	}
	handler := newHandler()
	input := operator.ActivateInput{ActivationRouteInput: operator.ActivationRouteInput{AgentID: change.AgentID.String(), Reason: "reviewed improvement"}, VersionID: change.Version.ID.String(), VersionDigest: change.Version.Digest, DecisionID: change.DecisionID.String()}
	call := func(c context.Context, action, key string, payload any, status int) opt.Activation {
		response := optimizationPOST(c, t, handler, "/activations/"+action, []string{key}, payload)
		if response.Code != status {
			t.Fatalf("%s: %d want %d: %s", action, response.Code, status, response.Body)
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s response can be cached: %d %s", action, response.Code, response.Body)
		}
		var out opt.Activation
		if status == http.StatusOK {
			if decodeErr := json.Unmarshal(response.Body.Bytes(), &out); decodeErr != nil {
				t.Fatal(decodeErr)
			}
		}
		return out
	}
	for _, missing := range []struct {
		action string
		input  any
	}{
		{"activate", input},
		{"rollback", operator.RollbackInput{ActivationRouteInput: input.ActivationRouteInput, TargetID: change.ID.String()}},
		{"deactivate", input.ActivationRouteInput},
	} {
		call(ctx, missing.action, "", missing.input, 400)
	}
	call(context.Background(), "activate", "activate", input, 403)
	auth.denied = true
	call(ctx, "activate", "activate", input, 403)
	auth.denied = false
	compatibility.stale = true
	call(ctx, "activate", "activate", input, 409)
	compatibility.stale = false
	foreign := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "foreign"}}})
	call(foreign, "activate", "activate", input, 404)
	active := call(ctx, "activate", "activate", input, 200)
	if active.Generation != 1 {
		t.Fatalf("activation: %+v", active)
	}
	if err = repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo = openOptimizationHTTPStore(t, path)
	handler = newHandler()
	retry := call(ctx, "activate", "activate", input, 200)
	if retry.ID != active.ID || retry.Generation != 1 {
		t.Fatal("restart repeated activation")
	}
	auth.denied = true
	call(ctx, "activate", "activate", input, 403)
	auth.denied = false
	altered := input
	altered.Reason = "different review"
	call(ctx, "activate", "activate", altered, 409)
	rollback := operator.RollbackInput{ActivationRouteInput: input.ActivationRouteInput, TargetID: active.ID.String()}
	rollback.ExpectedGeneration = 1
	call(ctx, "rollback", "activate", rollback, 409)
	rolled := call(ctx, "rollback", "rollback", rollback, 200)
	if rolled.Generation != 2 || rolled.RollbackTo != active.ID {
		t.Fatalf("rollback: %+v", rolled)
	}
	disable := input.ActivationRouteInput
	disable.ExpectedGeneration = 2
	call(ctx, "deactivate", "rollback", disable, 409)
	disabled := call(ctx, "deactivate", "deactivate", disable, 200)
	if !disabled.Deactivated || disabled.Generation != 3 {
		t.Fatalf("deactivate: %+v", disabled)
	}
	if err = repo.Close(); err != nil {
		t.Fatal(err)
	}
	repo = openOptimizationHTTPStore(t, path)
	handler = newHandler()
	retry = call(ctx, "deactivate", "deactivate", disable, 200)
	if retry.ID != disabled.ID || retry.Generation != 3 {
		t.Fatal("restart repeated deactivation")
	}
	call(ctx, "activate", "activate", input, 200)
	history := optimizationGET(ctx, handler, "/activations?agent_id="+input.AgentID)
	if history.Code != 200 {
		t.Fatalf("history: %d %s", history.Code, history.Body)
	}
	var page opt.ResourcePage[opt.Activation]
	if err = json.Unmarshal(history.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 || !page.Complete {
		t.Fatalf("history after retries: %+v", page)
	}
	if _, readErr := repo.GetActivation(ctx, change.AgentID, ""); !errors.Is(readErr, opt.ErrInactiveActivation) {
		t.Fatalf("old retry revived route: %v", readErr)
	}
	current, err := repo.GetActivationGeneration(ctx, disabled.ID)
	if err != nil || current.Generation != 3 {
		t.Fatalf("saved disable: %+v %v", current, err)
	}
}
