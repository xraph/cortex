package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/forge"
)

func (a *API) registerMemoryRoutes(router forge.Router) {
	g := router.Group("/cortex", forge.WithGroupTags("memory"))

	_ = g.GET("/agents/:name/memory", a.getConversation,
		forge.WithSummary("Get conversation"),
		forge.WithDescription("Returns conversation history for an agent."),
		forge.WithOperationID("getConversation"),
		forge.WithRequestSchema(GetConversationRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Conversation messages", []memory.Message{}),
		forge.WithErrorResponses(),
	)

	_ = g.DELETE("/agents/:name/memory", a.clearConversation,
		forge.WithSummary("Clear conversation"),
		forge.WithDescription("Clears conversation history for an agent."),
		forge.WithOperationID("clearConversation"),
		forge.WithNoContentResponse(),
		forge.WithErrorResponses(),
	)
}

func (a *API) getConversation(ctx forge.Context, req *GetConversationRequest) ([]memory.Message, error) {
	appID := cortex.AppFromContext(ctx.Context())
	cfg, err := a.eng.GetAgentByName(ctx.Context(), appID, ctx.Param("name"))
	if err != nil {
		return nil, mapStoreError(err)
	}

	tenantID := cortex.TenantFromContext(ctx.Context())
	limit := defaultLimit(req.Limit)

	messages, err := a.eng.LoadConversation(ctx.Context(), cfg.ID, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("load conversation: %w", err)
	}
	return messages, ctx.JSON(http.StatusOK, messages)
}

func (a *API) clearConversation(ctx forge.Context, _ *ClearConversationRequest) (*struct{}, error) {
	appID := cortex.AppFromContext(ctx.Context())
	cfg, err := a.eng.GetAgentByName(ctx.Context(), appID, ctx.Param("name"))
	if err != nil {
		return nil, mapStoreError(err)
	}

	tenantID := cortex.TenantFromContext(ctx.Context())

	if err := a.eng.ClearConversation(ctx.Context(), cfg.ID, tenantID); err != nil {
		return nil, fmt.Errorf("clear conversation: %w", err)
	}

	return nil, ctx.NoContent(http.StatusNoContent)
}
