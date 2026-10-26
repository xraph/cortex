package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/forge"

	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/operator"
)

func (a *API) registerOptimizationActivationCommands(router forge.Router) error {
	commands := operator.Commands{Optimizer: a.optimization}
	group := router.Group("/v1/optimization/activations", forge.WithGroupTags("optimization"))
	for _, route := range []struct {
		action, name   string
		handler, input any
	}{
		{"activate", "activateOptimizationVersion", optimizationMutation(commands.Activate), operator.ActivateInput{}},
		{"rollback", "rollbackOptimizationActivation", optimizationMutation(commands.Rollback), operator.RollbackInput{}},
		{"deactivate", "deactivateOptimizationRoute", optimizationMutation(commands.Deactivate), operator.ActivationRouteInput{}},
	} {
		if err := group.POST("/"+route.action, route.handler, forge.WithOperationID(route.name), forge.WithRequestSchema(route.input), forge.WithResponseSchema(http.StatusOK, "Immutable activation command receipt", opt.Activation{}), forge.WithErrorResponses()); err != nil {
			return fmt.Errorf("register optimization activation command %s: %w", route.name, err)
		}
	}
	return nil
}
