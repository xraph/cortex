package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/forge"

	"github.com/xraph/cortex"
)

// UpdateConfigRequest is the request body for updating engine configuration.
type UpdateConfigRequest struct {
	DefaultModel         string  `json:"default_model,omitempty"`
	DefaultMaxSteps      int     `json:"default_max_steps,omitempty"`
	DefaultMaxTokens     int     `json:"default_max_tokens,omitempty"`
	DefaultTemperature   float64 `json:"default_temperature,omitempty"`
	DefaultReasoningLoop string  `json:"default_reasoning_loop,omitempty"`
}

func (a *API) registerConfigRoutes(router forge.Router) error {
	g := router.Group("/v1", forge.WithGroupTags("config"))

	if err := g.GET("/config", a.getConfig,
		forge.WithSummary("Get config"),
		forge.WithDescription("Returns the current engine configuration."),
		forge.WithOperationID("getConfig"),
		forge.WithResponseSchema(http.StatusOK, "Engine configuration", &cortex.Config{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register config routes: %w", err)
	}

	if err := g.PUT("/config", a.updateConfig,
		forge.WithSummary("Update config"),
		forge.WithDescription("Updates engine configuration at runtime. Changes are in-memory only."),
		forge.WithOperationID("updateConfig"),
		forge.WithRequestSchema(UpdateConfigRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Updated configuration", &cortex.Config{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register config routes: %w", err)
	}

	return nil
}

func (a *API) getConfig(ctx forge.Context, _ *struct{}) (*cortex.Config, error) {
	cfg := a.eng.Config()
	return &cfg, ctx.JSON(http.StatusOK, cfg)
}

func (a *API) updateConfig(ctx forge.Context, req *UpdateConfigRequest) (*cortex.Config, error) {
	update := cortex.Config{
		DefaultModel:         req.DefaultModel,
		DefaultMaxSteps:      req.DefaultMaxSteps,
		DefaultMaxTokens:     req.DefaultMaxTokens,
		DefaultTemperature:   req.DefaultTemperature,
		DefaultReasoningLoop: req.DefaultReasoningLoop,
	}

	cfg := a.eng.UpdateConfig(update)
	return &cfg, ctx.JSON(http.StatusOK, cfg)
}
