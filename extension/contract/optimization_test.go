package contract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	dashauth "github.com/xraph/forge/extensions/dashboard/auth"
	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/storetest"
	"github.com/xraph/cortex/store/scopespy"
)

type optimizationTestAuthority struct{ denied bool }

func (a *optimizationTestAuthority) Authorize(c context.Context, _ opt.Action, _ opt.Resource) error {
	p, ok := cortex.PrincipalFromContext(c).(dc.Principal)
	if a.denied || !ok || p.User == nil || p.User.Subject != "fixture" {
		return fmt.Errorf("SECRET grant: %w", opt.ErrPolicyDenied)
	}
	return nil
}
func (*optimizationTestAuthority) Current(c context.Context) (opt.IdentityRef, error) {
	p, ok := cortex.PrincipalFromContext(c).(dc.Principal)
	if !ok || p.User == nil {
		return opt.IdentityRef{}, opt.ErrPolicyDenied
	}
	return opt.IdentityRef{Subject: p.User.Subject}, nil
}
func (*optimizationTestAuthority) Resolve(c context.Context, _ opt.IdentityRef, _ cortex.Scope) (context.Context, error) {
	return c, nil
}
func (*optimizationTestAuthority) Execute(context.Context, opt.WorkItem) (opt.Transition, error) {
	return opt.Transition{}, opt.ErrUnavailable
}

func TestOptimizationReadContracts(t *testing.T) {
	repo, err := memory.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	bundle := storetest.Fixture(t, "contract")
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	if err = repo.Transact(ctx, func(tx opt.Transaction) error { return tx.CreateExperiment(bundle) }); err != nil {
		t.Fatal(err)
	}
	auth := &optimizationTestAuthority{}
	service, err := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Caller: auth, Authorizer: auth, IdentityResolver: auth, Handler: auth, Worker: opt.DefaultWorkerConfig()})
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(engine.WithStore(scopespy.New()))
	if err != nil {
		t.Fatal(err)
	}
	create := func(value *opt.Optimizer) *dispatcher.Dispatcher {
		d := dispatcher.New(nil)
		if err = Register(d, dc.NewRegistry(), dc.NewWardenRegistry(), Deps{Engine: e, Optimization: value}); err != nil {
			t.Fatal(err)
		}
		return d
	}
	d, unavailable := create(service), create(nil)
	p := dc.Principal{User: &dashauth.UserInfo{Subject: "fixture", Scopes: []string{"cortex.optimization.read"}}, Claims: map[string]any{"cortex_scope": bundle.Experiment.Spec.Scope}}
	key := bundle.Experiment.ID.String()
	cases := []struct {
		intent  string
		payload any
		want    error
	}{
		{"optimization.experiments.list", map[string]any{}, nil},
		{"optimization.experiments.detail", map[string]any{"id": key}, nil},
		{"optimization.candidates.list", map[string]any{"id": key}, nil},
		{"optimization.candidates.detail", map[string]any{"experiment_id": key, "id": id.New(opt.PrefixCandidate).String()}, dc.ErrNotFound},
		{"optimization.trials.list", map[string]any{"id": key}, nil},
		{"optimization.trials.detail", map[string]any{"experiment_id": key, "id": id.New(opt.PrefixTrial).String()}, dc.ErrNotFound},
		{"optimization.comparison", map[string]any{"id": key}, nil},
		{"optimization.events.list", map[string]any{"id": key}, nil},
		{"optimization.versions.list", map[string]any{}, nil},
		{"optimization.versions.detail", map[string]any{"id": bundle.Versions[0].ID.String()}, nil},
		{"optimization.lessons.list", map[string]any{}, nil},
		{"optimization.lessons.detail", map[string]any{"id": id.New(opt.PrefixLesson).String()}, dc.ErrNotFound},
		{"optimization.publications.list", map[string]any{}, nil},
		{"optimization.activations.list", map[string]any{"agent_id": bundle.Experiment.Spec.AgentID.String()}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.intent, func(t *testing.T) {
			if _, callErr := dispatch(t, d, p, tc.intent, tc.payload); !errors.Is(callErr, tc.want) {
				t.Fatalf("authorized: %v", callErr)
			}
			if _, callErr := dispatch(t, unavailable, p, tc.intent, tc.payload); !errors.Is(callErr, dc.ErrUnavailable) {
				t.Fatalf("unconfigured: %v", callErr)
			}
			for _, scopes := range [][]string{nil, {"cortex.read"}, {"cortex.manage"}, {"cortex.run"}, {"cortex.optimization.manage"}, {"cortex.optimization.activate"}} {
				deniedP := p
				deniedP.User = &dashauth.UserInfo{Subject: "fixture", Scopes: scopes}
				if _, callErr := dispatch(t, d, deniedP, tc.intent, tc.payload); !errors.Is(callErr, dc.ErrPermissionDenied) {
					t.Fatalf("wrong permission %v: %v", scopes, callErr)
				}
			}
			anonymous := p
			anonymous.User = nil
			if _, callErr := dispatch(t, d, anonymous, tc.intent, tc.payload); !errors.Is(callErr, dc.ErrUnauthenticated) {
				t.Fatalf("anonymous: %v", callErr)
			}
			auth.denied = true
			_, callErr := dispatch(t, d, p, tc.intent, tc.payload)
			auth.denied = false
			if !errors.Is(callErr, dc.ErrPermissionDenied) || strings.Contains(callErr.Error(), "SECRET") {
				t.Fatalf("host denial: %v", callErr)
			}
		})
	}
	for _, tc := range []struct {
		intent  string
		payload any
	}{
		{"optimization.experiments.detail", map[string]any{"id": "bad"}},
		{"optimization.experiments.list", map[string]any{"limit": 101}},
		{"optimization.experiments.list", map[string]any{"agent_id": key}},
		{"optimization.events.list", map[string]any{"id": key, "cursor": "bad"}},
	} {
		if _, err = dispatch(t, d, p, tc.intent, tc.payload); !errors.Is(err, dc.ErrBadRequest) {
			t.Fatalf("invalid %s: %v", tc.intent, err)
		}
	}
	foreign := p
	foreign.Claims = map[string]any{"cortex_scope": cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "foreign"}}}}
	if _, err = dispatch(t, d, foreign, "optimization.experiments.detail", map[string]any{"id": key}); !errors.Is(err, dc.ErrNotFound) {
		t.Fatalf("foreign: %v", err)
	}
	malformed := p
	malformed.Claims = map[string]any{"cortex_scope": cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: ""}}}}
	if _, err = dispatch(t, d, malformed, "optimization.experiments.list", map[string]any{}); !errors.Is(err, dc.ErrPermissionDenied) {
		t.Fatalf("malformed: %v", err)
	}
	data, err := dispatch(t, d, foreign, "optimization.experiments.list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	var page map[string]any
	if err = json.Unmarshal(data, &page); err != nil {
		t.Fatal(err)
	}
	items, ok := page["items"].([]any)
	if !ok || len(items) != 0 || page["complete"] != true || page["total"] != nil {
		t.Fatalf("empty page: %s", data)
	}
}
