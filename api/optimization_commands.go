package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/xraph/forge"

	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/operator"
)

func (a *API) registerOptimizationCommands(router forge.Router) error {
	commands := operator.Commands{Optimizer: a.optimization}
	group := router.Group("/v1/optimization", forge.WithGroupTags("optimization"))
	for _, route := range []struct {
		action, name string
		invoke       func(context.Context, operator.LifecycleInput, string) (opt.Experiment, error)
	}{
		{"start", "startOptimizationExperiment", commands.Start},
		{"cancel", "cancelOptimizationExperiment", commands.Cancel},
		{"resume", "resumeOptimizationExperiment", commands.Resume},
	} {
		if err := group.POST("/experiments/:id/"+route.action, optimizationCommand(route.invoke), forge.WithOperationID(route.name), forge.WithRequestSchema(operator.LifecycleInput{}), forge.WithResponseSchema(http.StatusOK, "Current experiment state", opt.Experiment{}), forge.WithErrorResponses()); err != nil {
			return fmt.Errorf("register optimization command %s: %w", route.name, err)
		}
	}
	if err := group.POST("/decisions/retain", optimizationValueMutation(commands.Retain), forge.WithOperationID("retainOptimizationBaseline"), forge.WithRequestSchema(operator.RetainInput{}), forge.WithResponseSchema(http.StatusOK, "Current experiment and retained baseline review", opt.Experiment{}), forge.WithErrorResponses()); err != nil {
		return fmt.Errorf("register optimization retain command: %w", err)
	}
	if err := a.registerOptimizationLessonCommands(router); err != nil {
		return err
	}
	return a.registerOptimizationActivationCommands(router)
}

func optimizationCommand(invoke func(context.Context, operator.LifecycleInput, string) (opt.Experiment, error)) func(forge.Context, *operator.LifecycleInput) (*opt.Experiment, error) {
	return optimizationPathMutation(invoke, func(in operator.LifecycleInput, key string) operator.LifecycleInput { in.ID = key; return in })
}

func optimizationPathMutation[I, O any](invoke func(context.Context, I, string) (O, error), identify func(I, string) I) func(forge.Context, *I) (*O, error) {
	handler := optimizationValueMutation(invoke)
	return func(ctx forge.Context, in *I) (*O, error) {
		input := identify(*in, ctx.Param("id"))
		return handler(ctx, &input)
	}
}

func optimizationValueMutation[I, O any](invoke func(context.Context, I, string) (O, error)) func(forge.Context, *I) (*O, error) {
	return optimizationMutation(func(ctx context.Context, in I, key string) (*O, error) {
		out, err := invoke(ctx, in, key)
		return &out, err
	})
}

func optimizationMutation[I, O any](invoke func(context.Context, I, string) (O, error)) func(forge.Context, *I) (O, error) {
	return func(ctx forge.Context, in *I) (O, error) {
		var zero O
		ctx.SetHeader("Cache-Control", "no-store")
		keys := ctx.Request().Header.Values("Idempotency-Key")
		if len(keys) != 1 || !opt.ValidOpaqueKey(keys[0]) {
			return zero, replyOptimizationFailure(ctx, opt.ErrInvalidSpec)
		}
		out, err := invoke(ctx.Context(), *in, keys[0])
		if err != nil {
			return zero, replyOptimizationFailure(ctx, err)
		}
		return out, nil
	}
}

func replyOptimizationFailure(ctx forge.Context, err error) error {
	failure := operator.Failure(err)
	return ctx.JSON(failure.Status, failure)
}
