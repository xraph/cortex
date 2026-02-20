package api

import (
	"net/http"

	"github.com/xraph/forge"
)

func (a *API) registerToolRoutes(router forge.Router) {
	g := router.Group("/cortex", forge.WithGroupTags("tools"))

	_ = g.GET("/tools", a.listTools,
		forge.WithSummary("List tools"),
		forge.WithDescription("Returns all registered tools."),
		forge.WithOperationID("listTools"),
		forge.WithErrorResponses(),
	)

	_ = g.GET("/tools/:name/schema", a.getToolSchema,
		forge.WithSummary("Get tool schema"),
		forge.WithDescription("Returns the JSON Schema for a specific tool."),
		forge.WithOperationID("getToolSchema"),
		forge.WithErrorResponses(),
	)
}

func (a *API) listTools(ctx forge.Context, _ *ListToolsRequest) ([]map[string]any, error) {
	// TODO: implement tool registry in phase 2
	return []map[string]any{}, ctx.JSON(http.StatusOK, []map[string]any{})
}

func (a *API) getToolSchema(ctx forge.Context, _ *GetToolSchemaRequest) (*struct{}, error) {
	// TODO: implement tool schema lookup in phase 2
	_ = ctx.Param("name")
	return nil, forge.NotFound("tool schema not yet implemented")
}
