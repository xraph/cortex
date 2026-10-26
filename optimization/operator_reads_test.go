package optimization_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/storetest"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/scopespy"
)

func TestOperatorEvidenceCannotBeReadFromOptimizationExecution(t *testing.T) {
	for _, purpose := range []run.ExecutionPurpose{run.PurposeNormal, run.PurposeOptimizationTrial, run.PurposeOptimizationController, run.PurposeEvaluation} {
		t.Run(string(purpose), func(t *testing.T) {
			repository, err := memory.New()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = repository.Close() })
			bundle := storetest.Fixture(t, "operator-read")
			ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
			op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: repository, Authorizer: meterAuthorizer(func(context.Context, optimization.Action, optimization.Resource) error { return nil }), IdentityResolver: meterResolver(func(c context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
				return c, nil
			}), Caller: callerIdentity(func(context.Context) (optimization.IdentityRef, error) { return bundle.Experiment.Spec.Requester, nil }), Handler: phaseHandler(func(context.Context, optimization.WorkItem) (optimization.Transition, error) {
				return optimization.Transition{}, optimization.ErrUnavailable
			}), Worker: optimization.DefaultWorkerConfig()})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = op.Create(ctx, bundle); err != nil {
				t.Fatal(err)
			}
			calls := 0
			model := modelClient{complete: func(callCtx context.Context, _ *llm.Request) (*llm.Response, error) {
				calls++
				reads := []func() error{
					func() error { _, e := op.Get(callCtx, bundle.Experiment.ID); return e },
					func() error { _, e := op.List(callCtx, optimization.ListQuery{}); return e },
					func() error { _, e := op.GetVersion(callCtx, bundle.Versions[0].ID); return e },
					func() error {
						_, e := op.GetComparison(callCtx, bundle.Experiment.ID, optimization.PageQuery{})
						return e
					},
				}
				for _, read := range reads {
					readErr := read()
					if purpose == run.PurposeNormal {
						if readErr != nil {
							t.Error(readErr)
						}
					} else if !errors.Is(readErr, optimization.ErrPolicyDenied) {
						t.Errorf("private operator read reached %s: %v", purpose, readErr)
					}
				}

				if purpose != run.PurposeNormal {
					command := optimization.CommandRequest{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 1, IdempotencyKey: "private-command"}
					commands := []func() (optimization.Experiment, error){
						func() (optimization.Experiment, error) { return op.StartCommand(callCtx, command) },
						func() (optimization.Experiment, error) { return op.CancelCommand(callCtx, command) },
						func() (optimization.Experiment, error) { return op.ResumeCommand(callCtx, command) },
						func() (optimization.Experiment, error) { return op.Start(callCtx, bundle.Experiment.ID, 1) },
					}
					for _, invoke := range commands {
						if _, commandErr := invoke(); !errors.Is(commandErr, optimization.ErrPolicyDenied) {
							t.Errorf("private lifecycle command reached %s: %v", purpose, commandErr)
						}
					}
				}
				return &llm.Response{Content: "done", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 1}}, nil
			}}
			runtime, err := engine.New(engine.WithStore(scopespy.New()), engine.WithLLM(model), engine.WithModelRevision("fixture", "1"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = runtime.RunPrepared(ctx, bundle.Versions[0].Prepared, "work", engine.PreparedRunOptions{Purpose: purpose}); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("model calls: %d", calls)
			}
		})
	}
}
