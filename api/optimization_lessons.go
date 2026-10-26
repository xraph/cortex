package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/forge"

	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/operator"
)

func (a *API) registerOptimizationLessonCommands(router forge.Router) error {
	commands := operator.Commands{Optimizer: a.optimization}
	group := router.Group("/v1/optimization/lessons", forge.WithGroupTags("optimization"))
	retry := optimizationPathMutation(commands.RetryLesson, func(in operator.RetryLessonInput, id string) operator.RetryLessonInput { in.ID = id; return in })
	if err := group.POST("/:id/retry", retry, forge.WithOperationID("retryOptimizationLesson"), forge.WithRequestSchema(operator.RetryLessonInput{}), forge.WithResponseSchema(http.StatusOK, "Current local publication state", opt.Publication{}), forge.WithErrorResponses()); err != nil {
		return fmt.Errorf("register optimization lesson retry: %w", err)
	}
	revoke := optimizationPathMutation(commands.RevokeLesson, func(in operator.RevokeLessonInput, id string) operator.RevokeLessonInput { in.ID = id; return in })
	if err := group.POST("/:id/revoke", revoke, forge.WithOperationID("revokeOptimizationLesson"), forge.WithRequestSchema(operator.RevokeLessonInput{}), forge.WithResponseSchema(http.StatusOK, "Current local lesson eligibility", opt.Lesson{}), forge.WithErrorResponses()); err != nil {
		return fmt.Errorf("register optimization lesson revocation: %w", err)
	}
	return nil
}
