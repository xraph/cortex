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
	return nil
}

func optimizationCommand(invoke func(context.Context, operator.LifecycleInput, string) (opt.Experiment, error)) func(forge.Context, *operator.LifecycleInput) (*opt.Experiment, error) {
	return func(ctx forge.Context, in *operator.LifecycleInput) (*opt.Experiment, error) {
		ctx.SetHeader("Cache-Control", "no-store")
		keys := ctx.Request().Header.Values("Idempotency-Key")
		if len(keys) != 1 || !opt.ValidOpaqueKey(keys[0]) {
			return nil, replyOptimizationFailure(ctx, opt.ErrInvalidSpec)
		}
		input := *in
		input.ID = ctx.Param("id")
		result, err := invoke(ctx.Context(), input, keys[0])
		if err != nil {
			return nil, replyOptimizationFailure(ctx, err)
		}
		return &result, nil
	}
}

func replyOptimizationFailure(ctx forge.Context, err error) error {
	failure := operator.Failure(err)
	return ctx.JSON(failure.Status, failure)
}
