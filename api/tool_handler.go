package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/forge"
)

func (a *API) registerToolRoutes(router forge.Router) error {
	g := router.Group("/v1", forge.WithGroupTags("tools"))

	if err := g.GET("/tools", a.listTools,
		forge.WithSummary("List tools"),
		forge.WithDescription("Returns every tool the engine can offer a model, in dispatch order: builtins, registered tools, then external tools."),
		forge.WithResponseSchema(http.StatusOK, "Tool catalog", ListToolsResponse{}),
		a.op("listTools", false),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register tool routes: %w", err)
	}

	if err := g.GET("/tools/:name/schema", a.getToolSchema,
		forge.WithSummary("Get tool schema"),
		forge.WithDescription("Returns the JSON Schema of a tool's parameters."),
		forge.WithRequestSchema(GetToolSchemaRequest{}),
		a.op("getToolSchema", false),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register tool routes: %w", err)
	}

	return nil
}

func (a *API) listTools(_ forge.Context, _ *ListToolsRequest) (*ListToolsResponse, error) {
	resp := &ListToolsResponse{Items: []ToolResponse{}}
	if a.eng != nil {
		for _, info := range a.eng.Tools() {
			resp.Items = append(resp.Items, ToolResponse{
				Name:        info.Def.Name,
				Description: info.Def.Description,
				Kind:        string(info.Kind),
				RetrySafe:   info.RetrySafe,
			})
		}
	}
	// Returned rather than written: forge serializes the result, and
	// writing it here as well would send the body twice.
	return resp, nil
}

// getToolSchema serves the named tool's parameters schema as the engine
// advertises it to a model.
func (a *API) getToolSchema(ctx forge.Context, req *GetToolSchemaRequest) (*struct{}, error) {
	if a.eng != nil {
		for _, info := range a.eng.Tools() {
			if info.Def.Name != req.Name {
				continue
			}
			params := info.Def.Parameters
			if params == nil {
				params = map[string]any{}
			}
			return nil, ctx.JSON(http.StatusOK, params)
		}
	}
	return nil, forge.NotFound(fmt.Sprintf("tool %q not found", req.Name))
}
