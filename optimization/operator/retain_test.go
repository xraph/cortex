package operator_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/operator"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestRetainCommandAdapter(t *testing.T) {
	repo, ctx, activation := storetest.ReviewedActivationFixture(t, func(t *testing.T, now func() time.Time) store.Store {
		s, err := memory.New(memory.WithClock(now))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
	decision, err := repo.GetDecision(ctx, activation.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := repo.GetExperiment(ctx, decision.ExperimentID)
	if err != nil {
		t.Fatal(err)
	}
	auth := &authority{}
	service, err := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Authorizer: auth, Caller: auth, IdentityResolver: retainUnavailableResolver{}, Handler: auth, Worker: opt.DefaultWorkerConfig()})
	if err != nil {
		t.Fatal(err)
	}
	commands := operator.Commands{Optimizer: service}
	input := operator.RetainInput{ID: current.ID.String(), ExpectedRevision: current.Revision, DecisionID: decision.ID.String(), Reason: "Keep current baseline"}
	bad := input
	bad.ID = activation.Version.ID.String()
	if _, err = commands.Retain(ctx, bad, "review"); !errors.Is(err, opt.ErrInvalidSpec) {
		t.Fatalf("wrong experiment kind: %v", err)
	}
	bad = input
	bad.DecisionID = input.ID
	if _, err = commands.Retain(ctx, bad, "review"); !errors.Is(err, opt.ErrInvalidSpec) {
		t.Fatalf("wrong decision kind: %v", err)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	payload["idempotency_key"] = "forged"
	raw, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var forged operator.RetainInput
	if err = json.Unmarshal(raw, &forged); err != nil {
		t.Fatal(err)
	}
	if _, err = commands.Retain(ctx, forged, ""); !errors.Is(err, opt.ErrInvalidSpec) {
		t.Fatalf("payload supplied command identity: %v", err)
	}
	retained, err := commands.Retain(ctx, input, "review")
	if err != nil || retained.State != opt.StateCompleted || retained.RetainedBaseline == nil {
		t.Fatalf("retain with unavailable requester: %+v %v", retained, err)
	}
	again, err := commands.Retain(ctx, input, "review")
	if err != nil || again.Revision != retained.Revision {
		t.Fatalf("retry: %+v %v", again, err)
	}
	auth.denied = true
	if _, err = commands.Retain(ctx, input, "review"); !errors.Is(err, opt.ErrPolicyDenied) {
		t.Fatalf("retry skipped grant: %v", err)
	}
	if _, err = (operator.Commands{}).Retain(ctx, input, "review"); !errors.Is(err, opt.ErrUnavailable) {
		t.Fatalf("missing optimizer: %v", err)
	}
}

type retainUnavailableResolver struct{}

func (retainUnavailableResolver) Resolve(context.Context, opt.IdentityRef, cortex.Scope) (context.Context, error) {
	return nil, opt.ErrUnavailable
}
