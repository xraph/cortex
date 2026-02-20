package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/forge"
)

func (a *API) registerAgentRoutes(router forge.Router) {
	g := router.Group("/cortex", forge.WithGroupTags("agents"))

	_ = g.POST("/agents", a.createAgent,
		forge.WithSummary("Create agent"),
		forge.WithDescription("Creates a new agent with the specified configuration."),
		forge.WithOperationID("createAgent"),
		forge.WithRequestSchema(CreateAgentRequest{}),
		forge.WithCreatedResponse(&agent.Config{}),
		forge.WithErrorResponses(),
	)

	_ = g.GET("/agents", a.listAgents,
		forge.WithSummary("List agents"),
		forge.WithDescription("Returns agents with optional pagination."),
		forge.WithOperationID("listAgents"),
		forge.WithRequestSchema(ListAgentsRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Agent list", []*agent.Config{}),
		forge.WithErrorResponses(),
	)

	_ = g.GET("/agents/:name", a.getAgent,
		forge.WithSummary("Get agent"),
		forge.WithDescription("Returns details of a specific agent by name."),
		forge.WithOperationID("getAgent"),
		forge.WithResponseSchema(http.StatusOK, "Agent details", &agent.Config{}),
		forge.WithErrorResponses(),
	)

	_ = g.PUT("/agents/:name", a.updateAgent,
		forge.WithSummary("Update agent"),
		forge.WithDescription("Updates an existing agent configuration."),
		forge.WithOperationID("updateAgent"),
		forge.WithRequestSchema(UpdateAgentRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Updated agent", &agent.Config{}),
		forge.WithErrorResponses(),
	)

	_ = g.DELETE("/agents/:name", a.deleteAgent,
		forge.WithSummary("Delete agent"),
		forge.WithDescription("Deletes an agent configuration."),
		forge.WithOperationID("deleteAgent"),
		forge.WithNoContentResponse(),
		forge.WithErrorResponses(),
	)

	_ = g.POST("/agents/:name/run", a.runAgent,
		forge.WithSummary("Run agent"),
		forge.WithDescription("Executes an agent with the given input and returns the result."),
		forge.WithOperationID("runAgent"),
		forge.WithRequestSchema(RunAgentRequest{}),
		forge.WithErrorResponses(),
	)

	_ = g.POST("/agents/:name/stream", a.streamAgent,
		forge.WithSummary("Stream agent"),
		forge.WithDescription("Executes an agent with streaming SSE output."),
		forge.WithOperationID("streamAgent"),
		forge.WithRequestSchema(StreamAgentRequest{}),
		forge.WithErrorResponses(),
	)
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

func (a *API) listAgents(ctx forge.Context, req *ListAgentsRequest) ([]*agent.Config, error) {
	agents, err := a.eng.ListAgents(ctx.Context(), &agent.ListFilter{
		AppID:  cortex.AppFromContext(ctx.Context()),
		Limit:  defaultLimit(req.Limit),
		Offset: req.Offset,
	})
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}
	return agents, ctx.JSON(http.StatusOK, agents)
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

func (a *API) runAgent(_ forge.Context, _ *RunAgentRequest) (*struct{}, error) {
	// TODO: implement agent execution in phase 2
	return nil, forge.NotFound("agent execution not yet implemented")
}

func (a *API) streamAgent(_ forge.Context, _ *StreamAgentRequest) (*struct{}, error) {
	// TODO: implement streaming agent execution in phase 2
	return nil, forge.NotFound("agent streaming not yet implemented")
}
