package operator_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/operator"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestLifecycleCommandAdapters(t *testing.T) {
	repo, err := memory.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	auth := &authority{}
	service, err := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Authorizer: auth, Caller: auth, IdentityResolver: auth, Handler: auth, Worker: opt.DefaultWorkerConfig()})
	if err != nil {
		t.Fatal(err)
	}
	bundle := storetest.Fixture(t, "command-adapter")
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	if _, err = service.Create(ctx, bundle); err != nil {
		t.Fatal(err)
	}
	commands := operator.Commands{Optimizer: service}
	input := operator.LifecycleInput{ID: bundle.Experiment.ID.String(), ExpectedRevision: 1}
	for _, invoke := range []func(context.Context, operator.LifecycleInput, string) (opt.Experiment, error){commands.Start, commands.Cancel, commands.Resume} {
		if _, err = invoke(ctx, input, ""); !errors.Is(err, opt.ErrInvalidSpec) {
			t.Fatalf("missing envelope key: %v", err)
		}
		bad := input
		bad.ID = bundle.Versions[0].ID.String()
		if _, err = invoke(ctx, bad, "key"); !errors.Is(err, opt.ErrInvalidSpec) {
			t.Fatalf("wrong kind: %v", err)
		}
		auth.denied = true
		if _, err = invoke(ctx, input, "key"); !errors.Is(err, opt.ErrPolicyDenied) {
			t.Fatalf("denied: %v", err)
		}
		auth.denied = false
	}
	var forged operator.LifecycleInput
	if err = json.Unmarshal([]byte(`{"id":"`+input.ID+`","expected_revision":1,"idempotency_key":"forged"}`), &forged); err != nil {
		t.Fatal(err)
	}
	if _, err = commands.Start(ctx, forged, ""); !errors.Is(err, opt.ErrInvalidSpec) {
		t.Fatalf("payload supplied envelope key: %v", err)
	}
	current, err := commands.Start(ctx, input, "start")
	if err != nil || current.State != opt.StateQueued {
		t.Fatalf("start: %+v %v", current, err)
	}
	again, err := commands.Start(ctx, input, "start")
	if err != nil || again.Revision != current.Revision {
		t.Fatalf("retry: %+v %v", again, err)
	}
	if _, err = commands.Resume(ctx, input, "start"); !errors.Is(err, opt.ErrConflict) {
		t.Fatalf("action collision: %v", err)
	}
	input.ExpectedRevision = current.Revision
	current, err = commands.Cancel(ctx, input, "cancel")
	if err != nil || current.State != opt.StateCancelled {
		t.Fatalf("cancel: %+v %v", current, err)
	}
	if _, err = (operator.Commands{}).Start(ctx, input, "start"); !errors.Is(err, opt.ErrUnavailable) {
		t.Fatalf("unconfigured: %v", err)
	}
}
