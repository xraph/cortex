package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/xraph/forge"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/session"
)

func (a *API) registerMemoryRoutes(router forge.Router) error {
	g := router.Group("/v1", forge.WithGroupTags("memory"))

	if err := g.GET("/agents/:name/memory", a.getConversation,
		forge.WithSummary("Get conversation"),
		forge.WithDescription("Returns conversation history for an agent."),
		forge.WithOperationID("getConversation"),
		forge.WithRequestSchema(GetConversationRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Conversation messages", []memory.Message{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register memory routes: %w", err)
	}

	if err := g.DELETE("/agents/:name/memory", a.clearConversation,
		forge.WithSummary("Clear conversation"),
		forge.WithDescription("Clears conversation history for an agent."),
		forge.WithOperationID("clearConversation"),
		forge.WithNoContentResponse(),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register memory routes: %w", err)
	}

	return nil
}

func (a *API) getConversation(ctx forge.Context, req *GetConversationRequest) (*GetConversationResponse, error) {
	cfg, err := a.eng.GetAgentByName(ctx.Context(), ctx.Param("name"))
	if err != nil {
		return nil, mapStoreError(err)
	}

	sessionID, err := a.resolveConversationSession(ctx.Context(), cfg.ID, req.SessionID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	if sessionID.IsNil() {
		// No session has ever been created for this agent in this scope
		// (the agent has never run), so there is nothing to load yet.
		resp := &GetConversationResponse{Messages: []memory.Message{}}
		return resp, ctx.JSON(http.StatusOK, resp)
	}

	limit := defaultLimit(req.Limit)

	messages, err := a.eng.LoadConversation(ctx.Context(), cfg.ID, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("load conversation: %w", err)
	}
	resp := &GetConversationResponse{Messages: messages}
	return resp, ctx.JSON(http.StatusOK, resp)
}

func (a *API) clearConversation(ctx forge.Context, req *ClearConversationRequest) (*struct{}, error) {
	cfg, err := a.eng.GetAgentByName(ctx.Context(), ctx.Param("name"))
	if err != nil {
		return nil, mapStoreError(err)
	}

	sessionID, err := a.resolveConversationSession(ctx.Context(), cfg.ID, req.SessionID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	if sessionID.IsNil() {
		// Nothing has ever been saved for this agent in this scope, so
		// clearing it is a no-op rather than an error.
		return nil, ctx.NoContent(http.StatusNoContent)
	}

	if err := a.eng.ClearConversation(ctx.Context(), cfg.ID, sessionID); err != nil {
		return nil, fmt.Errorf("clear conversation: %w", err)
	}

	return nil, ctx.NoContent(http.StatusNoContent)
}

// resolveConversationSession resolves which session a memory API call
// should operate on. An explicit raw session id (from the request's
// session_id query param) wins; otherwise it looks up the agent's
// default session for the caller's scope, returning the zero id when
// none has been created yet. Unlike engine.resolveSession, this never
// lazily creates one: Get/ClearConversation are read and maintenance
// operations, not run-starting ones, and an agent that has never run has
// legitimately nothing to load or clear.
func (a *API) resolveConversationSession(ctx context.Context, agentID id.AgentID, raw string) (id.SessionID, error) {
	if raw != "" {
		sessionID, err := id.ParseSessionID(raw)
		if err != nil {
			return id.SessionID{}, fmt.Errorf("parse session_id: %w", err)
		}
		return sessionID, nil
	}

	sessions, err := a.eng.Store().ListSessions(ctx, &session.ListFilter{AgentID: agentID, Limit: 1, DefaultOnly: true})
	if err != nil {
		return id.SessionID{}, fmt.Errorf("resolve default session: %w", err)
	}
	if len(sessions) == 1 {
		return sessions[0].ID, nil
	}
	return id.SessionID{}, nil
}
