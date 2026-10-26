package extension

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	dashauth "github.com/xraph/forge/extensions/dashboard/auth"
	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	cortexcontract "github.com/xraph/cortex/extension/contract"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/store/scopespy"
)

type extensionOptimizationHost struct{}

func (extensionOptimizationHost) Authorize(context.Context, opt.Action, opt.Resource) error {
	return nil
}
func (extensionOptimizationHost) Current(context.Context) (opt.IdentityRef, error) {
	return opt.IdentityRef{Subject: "operator"}, nil
}
func (extensionOptimizationHost) Resolve(c context.Context, _ opt.IdentityRef, _ cortex.Scope) (context.Context, error) {
	return c, nil
}
func (extensionOptimizationHost) Execute(context.Context, opt.WorkItem) (opt.Transition, error) {
	return opt.Transition{}, errors.New("workers must be started by the host")
}

func TestOptimizationDashboardOption(t *testing.T) {
	repo, err := memory.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	host := extensionOptimizationHost{}
	service, err := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Authorizer: host, Caller: host, IdentityResolver: host, Handler: host, Worker: opt.DefaultWorkerConfig()})
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(engine.WithStore(scopespy.New()))
	if err != nil {
		t.Fatal(err)
	}
	deps := cortexcontract.Deps{DefaultScope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "a"}}}}
	for _, options := range [][]ExtOption{
		{WithOptimization(service), WithDashboard(deps)},
		{WithDashboard(deps), WithOptimization(service)},
		{WithDashboard(cortexcontract.Deps{DefaultScope: deps.DefaultScope, Optimization: service})},
	} {
		extension := New(options...)
		extension.eng = e
		d := dispatcher.New(nil)
		if err = extension.RegisterContractContributor(d, dc.NewRegistry(), dc.NewWardenRegistry()); err != nil {
			t.Fatal(err)
		}
		p := dc.Principal{User: &dashauth.UserInfo{Subject: "operator", Scopes: []string{"cortex.optimization.read"}}}
		data, _, callErr := d.Dispatch(context.Background(), dc.Request{Contributor: "cortex", Intent: "optimization.experiments.list", IntentVersion: 1, Kind: dc.KindQuery, Payload: json.RawMessage(`{}`)}, p)
		if callErr != nil {
			t.Fatal(callErr)
		}
		var page opt.ResourcePage[opt.Experiment]
		if err = json.Unmarshal(data, &page); err != nil {
			t.Fatal(err)
		}
		if page.Items == nil || len(page.Items) != 0 || !page.Complete {
			t.Fatalf("configured empty: %s", data)
		}
	}
}
