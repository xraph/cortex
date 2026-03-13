package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/forge"
)

func (a *API) registerToolRoutes(router forge.Router) error {
	g := router.Group("", forge.WithGroupTags("tools"))

	if err := g.GET("/tools", a.listTools,
		forge.WithSummary("List tools"),
		forge.WithDescription("Returns all registered tools."),
		forge.WithOperationID("listTools"),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register tool routes: %w", err)
	}

	if err := g.GET("/tools/:name/schema", a.getToolSchema,
		forge.WithSummary("Get tool schema"),
		forge.WithDescription("Returns the JSON Schema for a specific tool."),
		forge.WithOperationID("getToolSchema"),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register tool routes: %w", err)
	}

	return nil
}

func (a *API) listTools(ctx forge.Context, _ *ListToolsRequest) (*ListToolsResponse, error) {
	// TODO: implement tool registry in phase 2
	resp := &ListToolsResponse{Items: []map[string]any{}}
	return resp, ctx.JSON(http.StatusOK, resp)
}

func (a *API) getToolSchema(_ forge.Context, _ *GetToolSchemaRequest) (*struct{}, error) {
	// TODO: implement tool schema lookup in phase 2
	return nil, forge.NotFound("tool schema not yet implemented")
}
