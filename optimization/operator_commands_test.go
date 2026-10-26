package optimization_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestOperatorCommandRechecksCallerAfterRequesterResolution(t *testing.T) {
	for _, mode := range []string{"grant-withdrawn", "identity-changed"} {
		t.Run(mode, func(t *testing.T) {
			repo, err := memory.New()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = repo.Close() })
			bundle := storetest.Fixture(t, "command-authority")
			ctx := cortex.WithScope(context.WithValue(context.Background(), workerSubjectKey{}, "operator"), bundle.Experiment.Spec.Scope)
			if err = repo.Transact(ctx, func(tx optimization.Transaction) error { return tx.CreateExperiment(bundle) }); err != nil {
				t.Fatal(err)
			}
			resolved := false
			op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: repo, Authorizer: meterAuthorizer(func(c context.Context, _ optimization.Action, _ optimization.Resource) error {
				if resolved && mode == "grant-withdrawn" && c.Value(workerSubjectKey{}) == "operator" {
					return optimization.ErrPolicyDenied
				}
				return nil
			}), IdentityResolver: meterResolver(func(c context.Context, actor optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
				resolved = true
				return context.WithValue(c, workerSubjectKey{}, actor.Subject), nil
			}), Caller: callerIdentity(func(context.Context) (optimization.IdentityRef, error) {
				subject := "operator"
				if resolved && mode == "identity-changed" {
					subject = "different-operator"
				}
				return optimization.IdentityRef{Subject: subject}, nil
			}), Handler: phaseHandler(func(context.Context, optimization.WorkItem) (optimization.Transition, error) {
				t.Fatal("command executed a worker")
				return optimization.Transition{}, nil
			}), Worker: optimization.DefaultWorkerConfig()})
			if err != nil {
				t.Fatal(err)
			}
			_, err = op.StartCommand(ctx, optimization.CommandRequest{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 1, IdempotencyKey: "start"})
			if !errors.Is(err, optimization.ErrPolicyDenied) {
				t.Fatalf("requester resolution changed caller authority: %v", err)
			}
			current, err := repo.GetExperiment(ctx, bundle.Experiment.ID)
			if err != nil || current.State != optimization.StateDraft || current.Revision != 1 {
				t.Fatalf("unauthorized transition: %+v %v", current, err)
			}
		})
	}
}
