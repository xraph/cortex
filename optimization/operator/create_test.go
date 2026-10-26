package operator_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/operator"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/storetest"
)

type adapterCreationCatalog struct {
	bundle opt.ExperimentBundle
	calls  int
}

func (c *adapterCreationCatalog) Authorize(ctx context.Context, _ opt.CreationSelection) error {
	return ctx.Err()
}
func (c *adapterCreationCatalog) Prepare(ctx context.Context, target opt.CreationTarget) (opt.ExperimentBundle, error) {
	c.calls++
	result := c.bundle
	result.Experiment.ID = target.ExperimentID
	result.Experiment.Spec.Requester = target.Actor
	return result, ctx.Err()
}
func TestCreateAdapterUsesEnvelopeAndHostBundle(t *testing.T) {
	repo, err := memory.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	bundle := storetest.Fixture(t, "creation-adapter")
	catalog := &adapterCreationCatalog{bundle: bundle}
	auth := &authority{}
	service, err := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Caller: auth, Authorizer: auth, IdentityResolver: auth, Handler: auth, Worker: opt.DefaultWorkerConfig(), CreationCatalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	commands := operator.Commands{Optimizer: service}
	input := operator.CreateInput{Selection: opt.CreationSelection{Agent: opt.ContentRef{Ref: bundle.Experiment.Spec.AgentID.String(), Revision: "1", Digest: strings.Repeat("a", 64)}, Evaluation: bundle.Experiment.Spec.Evaluation, Controller: opt.ContentRef{Ref: bundle.Experiment.Spec.Controller.ID.String(), Revision: "1", Digest: bundle.Experiment.Spec.Controller.Digest}, Environment: bundle.Experiment.Spec.Tasks[0].EnvironmentSnapshot}}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var forged map[string]any
	if err = json.Unmarshal(encoded, &forged); err != nil {
		t.Fatal(err)
	}
	forged["idempotency_key"] = "forged-body-key"
	forged["bundle"] = map[string]any{"policy": "caller-policy", "requester": "forged-user"}
	encoded, err = json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(encoded, &input); err != nil {
		t.Fatal(err)
	}
	if _, err = commands.Create(ctx, input, ""); !errors.Is(err, opt.ErrInvalidSpec) || catalog.calls != 0 {
		t.Fatalf("body supplied command authority: %v", err)
	}
	created, err := commands.Create(ctx, input, "transport-key")
	if err != nil || created.Spec.Requester.Subject != "fixture" {
		t.Fatalf("creation: %+v %v", created, err)
	}
	again, err := commands.Create(ctx, input, "transport-key")
	if err != nil || again.ID != created.ID || catalog.calls != 1 {
		t.Fatalf("adapter duplicated preparation: %+v %v", again, err)
	}
	auth.denied = true
	if _, err = commands.Create(ctx, input, "transport-key"); !errors.Is(err, opt.ErrPolicyDenied) {
		t.Fatalf("current grant: %v", err)
	}
	if _, err = (operator.Commands{}).Create(ctx, input, "transport-key"); !errors.Is(err, opt.ErrUnavailable) {
		t.Fatalf("unavailable: %v", err)
	}
}
