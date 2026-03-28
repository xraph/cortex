package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/forge"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
)

func (a *API) registerAgentRoutes(router forge.Router) error {
	g := router.Group("/v1", forge.WithGroupTags("agents"))

	if err := g.POST("/agents", a.createAgent,
		forge.WithSummary("Create agent"),
		forge.WithDescription("Creates a new agent with the specified configuration."),
		forge.WithOperationID("createAgent"),
		forge.WithRequestSchema(CreateAgentRequest{}),
		forge.WithCreatedResponse(&agent.Config{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register agent routes: %w", err)
	}

	if err := g.GET("/agents", a.listAgents,
		forge.WithSummary("List agents"),
		forge.WithDescription("Returns agents with optional pagination."),
		forge.WithOperationID("listAgents"),
		forge.WithRequestSchema(ListAgentsRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Agent list", []*agent.Config{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register agent routes: %w", err)
	}

	if err := g.GET("/agents/:name", a.getAgent,
		forge.WithSummary("Get agent"),
		forge.WithDescription("Returns details of a specific agent by name."),
		forge.WithOperationID("getAgent"),
		forge.WithResponseSchema(http.StatusOK, "Agent details", &agent.Config{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register agent routes: %w", err)
	}

	if err := g.PUT("/agents/:name", a.updateAgent,
		forge.WithSummary("Update agent"),
		forge.WithDescription("Updates an existing agent configuration."),
		forge.WithOperationID("updateAgent"),
		forge.WithRequestSchema(UpdateAgentRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Updated agent", &agent.Config{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register agent routes: %w", err)
	}

	if err := g.DELETE("/agents/:name", a.deleteAgent,
		forge.WithSummary("Delete agent"),
		forge.WithDescription("Deletes an agent configuration."),
		forge.WithOperationID("deleteAgent"),
		forge.WithNoContentResponse(),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register agent routes: %w", err)
	}

	if err := g.POST("/agents/:name/run", a.runAgent,
		forge.WithSummary("Run agent"),
		forge.WithDescription("Executes an agent with the given input and returns the result."),
		forge.WithOperationID("runAgent"),
		forge.WithRequestSchema(RunAgentRequest{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register agent routes: %w", err)
	}

	if err := g.POST("/agents/:name/stream", a.streamAgent,
		forge.WithSummary("Stream agent"),
		forge.WithDescription("Executes an agent with streaming SSE output."),
		forge.WithOperationID("streamAgent"),
		forge.WithRequestSchema(StreamAgentRequest{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register agent routes: %w", err)
	}

	if err := g.POST("/agents/:name/preview-prompt", a.previewPrompt,
		forge.WithSummary("Preview prompt"),
		forge.WithDescription("Returns the computed system prompt for the agent without executing."),
		forge.WithOperationID("previewPrompt"),
		forge.WithResponseSchema(http.StatusOK, "Computed prompt", &PreviewPromptResponse{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register agent routes: %w", err)
	}

	return nil
}

func (a *API) createAgent(ctx forge.Context, req *CreateAgentRequest) (*agent.Config, error) {
	if req.Name == "" {
		return nil, forge.BadRequest("name is required")
	}

	cfg := &agent.Config{
		Entity:          cortex.NewEntity(),
		ID:              id.NewAgentID(),
		Name:            req.Name,
		Description:     req.Description,
		AppID:           cortex.AppFromContext(ctx.Context()),
		SystemPrompt:    req.SystemPrompt,
		Model:           req.Model,
		Tools:           req.Tools,
		MaxSteps:        req.MaxSteps,
		MaxTokens:       req.MaxTokens,
		Temperature:     req.Temperature,
		ReasoningLoop:   req.ReasoningLoop,
		PersonaRef:      req.PersonaRef,
		InlineSkills:    req.InlineSkills,
		InlineTraits:    req.InlineTraits,
		InlineBehaviors: req.InlineBehaviors,
		Guardrails:      req.Guardrails,
		Metadata:        req.Metadata,
		Enabled:         true,
	}

	if err := a.eng.CreateAgent(ctx.Context(), cfg); err != nil {
		return nil, fmt.Errorf("create agent: %w", err)
	}

	return cfg, ctx.JSON(http.StatusCreated, cfg)
}

func (a *API) getAgent(ctx forge.Context, _ *GetAgentRequest) (*agent.Config, error) {
	cfg, err := a.eng.GetAgentByName(ctx.Context(), cortex.AppFromContext(ctx.Context()), ctx.Param("name"))
	if err != nil {
		return nil, mapStoreError(err)
	}
	return cfg, ctx.JSON(http.StatusOK, cfg)
}

func (a *API) listAgents(ctx forge.Context, req *ListAgentsRequest) (*ListAgentsResponse, error) {
	agents, err := a.eng.ListAgents(ctx.Context(), &agent.ListFilter{
		AppID:  cortex.AppFromContext(ctx.Context()),
		Limit:  defaultLimit(req.Limit),
		Offset: req.Offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	resp := &ListAgentsResponse{Items: agents}
	return resp, ctx.JSON(http.StatusOK, resp)
}

func (a *API) updateAgent(ctx forge.Context, req *UpdateAgentRequest) (*agent.Config, error) {
	cfg, err := a.eng.GetAgentByName(ctx.Context(), cortex.AppFromContext(ctx.Context()), req.Name)
	if err != nil {
		return nil, mapStoreError(err)
	}

	if req.Description != "" {
		cfg.Description = req.Description
	}
	if req.SystemPrompt != "" {
		cfg.SystemPrompt = req.SystemPrompt
	}
	if req.Model != "" {
		cfg.Model = req.Model
	}
	if req.Tools != nil {
		cfg.Tools = req.Tools
	}
	if req.MaxSteps > 0 {
		cfg.MaxSteps = req.MaxSteps
	}
	if req.MaxTokens > 0 {
		cfg.MaxTokens = req.MaxTokens
	}
	if req.Temperature > 0 {
		cfg.Temperature = req.Temperature
	}
	if req.ReasoningLoop != "" {
		cfg.ReasoningLoop = req.ReasoningLoop
	}
	if req.PersonaRef != "" {
		cfg.PersonaRef = req.PersonaRef
	}
	if req.InlineSkills != nil {
		cfg.InlineSkills = req.InlineSkills
	}
	if req.InlineTraits != nil {
		cfg.InlineTraits = req.InlineTraits
	}
	if req.InlineBehaviors != nil {
		cfg.InlineBehaviors = req.InlineBehaviors
	}
	if req.Guardrails != nil {
		cfg.Guardrails = req.Guardrails
	}
	if req.Metadata != nil {
		cfg.Metadata = req.Metadata
	}

	if err := a.eng.UpdateAgent(ctx.Context(), cfg); err != nil {
		return nil, fmt.Errorf("update agent: %w", err)
	}
	return cfg, ctx.JSON(http.StatusOK, cfg)
}

func (a *API) deleteAgent(ctx forge.Context, _ *DeleteAgentRequest) (*struct{}, error) {
	cfg, err := a.eng.GetAgentByName(ctx.Context(), cortex.AppFromContext(ctx.Context()), ctx.Param("name"))
	if err != nil {
		return nil, mapStoreError(err)
	}
	if err := a.eng.DeleteAgent(ctx.Context(), cfg.ID); err != nil {
		return nil, mapStoreError(err)
	}
	return nil, ctx.NoContent(http.StatusNoContent)
}

func (a *API) runAgent(ctx forge.Context, req *RunAgentRequest) (*RunAgentResponse, error) {
	if req.Input == "" {
		return nil, forge.BadRequest("input is required")
	}

	appID := cortex.AppFromContext(ctx.Context())
	r, err := a.eng.RunAgent(ctx.Context(), appID, req.Name, req.Input, mapOverrides(req.Overrides))
	if err != nil {
		return nil, mapStoreError(err)
	}

	var durationMs int64
	if r.StartedAt != nil && r.CompletedAt != nil {
		durationMs = r.CompletedAt.Sub(*r.StartedAt).Milliseconds()
	}

	resp := &RunAgentResponse{
		RunID:      r.ID.String(),
		Output:     r.Output,
		State:      string(r.State),
		StepCount:  r.StepCount,
		TokensUsed: r.TokensUsed,
		DurationMs: durationMs,
	}
	return resp, ctx.JSON(http.StatusOK, resp)
}

func (a *API) streamAgent(ctx forge.Context, req *StreamAgentRequest) (*struct{}, error) {
	if req.Input == "" {
		return nil, forge.BadRequest("input is required")
	}

	ctx.SetHeader("Content-Type", "text/event-stream")
	ctx.SetHeader("Cache-Control", "no-cache")
	ctx.SetHeader("Connection", "keep-alive")
	ctx.SetHeader("X-Accel-Buffering", "no")

	appID := cortex.AppFromContext(ctx.Context())
	events := make(chan engine.StreamEvent, 64)

	if err := a.eng.StreamAgent(ctx.Context(), appID, req.Name, req.Input, mapOverrides(req.Overrides), events); err != nil {
		return nil, mapStoreError(err)
	}

	for evt := range events {
		if err := ctx.WriteSSE(string(evt.Type), evt.Data); err != nil {
			break
		}
	}

	return nil, nil
}

func (a *API) previewPrompt(ctx forge.Context, _ *PreviewPromptRequest) (*PreviewPromptResponse, error) {
	appID := cortex.AppFromContext(ctx.Context())
	ag, err := a.eng.GetAgentByName(ctx.Context(), appID, ctx.Param("name"))
	if err != nil {
		return nil, mapStoreError(err)
	}

	// Use the engine's prompt builder for a consistent preview.
	prompt := a.eng.BuildSystemPrompt(ctx.Context(), ag, nil)

	resp := &PreviewPromptResponse{
		Prompt: prompt,
	}
	return resp, ctx.JSON(http.StatusOK, resp)
}

// mapOverrides converts API-layer overrides to engine-layer overrides.
func mapOverrides(o *AgentOverrides) *engine.RunOverrides {
	if o == nil {
		return nil
	}
	return &engine.RunOverrides{
		Model:           o.Model,
		Temperature:     o.Temperature,
		MaxSteps:        o.MaxSteps,
		MaxTokens:       o.MaxTokens,
		ReasoningLoop:   o.ReasoningLoop,
		SystemPrompt:    o.SystemPrompt,
		PersonaRef:      o.PersonaRef,
		InlineSkills:    o.InlineSkills,
		InlineTraits:    o.InlineTraits,
		InlineBehaviors: o.InlineBehaviors,
		Tools:           o.Tools,
	}
}
