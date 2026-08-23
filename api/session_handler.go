package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/xraph/forge"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/session"
)

func (a *API) registerSessionRoutes(router forge.Router) error {
	g := router.Group("/v1", forge.WithGroupTags("sessions"))

	if err := g.POST("/agents/:name/sessions", a.createSession,
		forge.WithSummary("Create session"),
		forge.WithDescription("Creates a new conversation session for an agent."),
		forge.WithOperationID("createSession"),
		forge.WithRequestSchema(CreateSessionRequest{}),
		forge.WithCreatedResponse(&session.Session{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register session routes: %w", err)
	}

	if err := g.GET("/agents/:name/sessions", a.listSessions,
		forge.WithSummary("List sessions"),
		forge.WithDescription("Returns an agent's sessions with optional pagination."),
		forge.WithOperationID("listSessions"),
		forge.WithRequestSchema(ListSessionsRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Session list", []*session.Session{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register session routes: %w", err)
	}

	// Registered ahead of the :id route below: bunrouter tries static
	// children before the colon (param) node at the same segment, so
	// "count" only ever resolves here and never reaches getSession with
	// req.SessionID == "count".
	if err := g.GET("/agents/:name/sessions/count", a.countSessions,
		forge.WithSummary("Count sessions"),
		forge.WithDescription("Returns the number of sessions an agent has."),
		forge.WithOperationID("countSessions"),
		forge.WithRequestSchema(CountSessionsRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Session count", &CountSessionsResponse{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register session routes: %w", err)
	}

	if err := g.GET("/agents/:name/sessions/:id", a.getSession,
		forge.WithSummary("Get session"),
		forge.WithOperationID("getSession"),
		forge.WithResponseSchema(http.StatusOK, "Session details", &session.Session{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register session routes: %w", err)
	}

	if err := g.PUT("/agents/:name/sessions/:id", a.updateSession,
		forge.WithSummary("Update session"),
		forge.WithOperationID("updateSession"),
		forge.WithRequestSchema(UpdateSessionRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Updated session", &session.Session{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register session routes: %w", err)
	}

	if err := g.DELETE("/agents/:name/sessions/:id", a.deleteSession,
		forge.WithSummary("Delete session"),
		forge.WithOperationID("deleteSession"),
		forge.WithNoContentResponse(),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register session routes: %w", err)
	}

	return nil
}

func (a *API) createSession(ctx forge.Context, req *CreateSessionRequest) (*session.Session, error) {
	cfg, err := a.eng.GetAgentByName(ctx.Context(), req.Name)
	if err != nil {
		return nil, mapStoreError(err)
	}

	s := &session.Session{
		Entity:   cortex.NewEntity(),
		ID:       id.NewSessionID(),
		AgentID:  cfg.ID,
		Title:    req.Title,
		Metadata: req.Metadata,
	}

	if err := a.eng.Store().CreateSession(ctx.Context(), s); err != nil {
		return nil, mapStoreError(err)
	}

	return s, ctx.JSON(http.StatusCreated, s)
}

func (a *API) getSession(ctx forge.Context, req *GetSessionRequest) (*session.Session, error) {
	cfg, err := a.eng.GetAgentByName(ctx.Context(), req.Name)
	if err != nil {
		return nil, mapStoreError(err)
	}

	sessionID, err := id.ParseSessionID(req.SessionID)
	if err != nil {
		return nil, forge.BadRequest(fmt.Sprintf("invalid session ID: %v", err))
	}

	sess, err := a.resolveOwnedSession(ctx.Context(), cfg.ID, sessionID)
	if err != nil {
		return nil, mapStoreError(err)
	}

	return sess, ctx.JSON(http.StatusOK, sess)
}

func (a *API) listSessions(ctx forge.Context, req *ListSessionsRequest) (*ListSessionsResponse, error) {
	cfg, err := a.eng.GetAgentByName(ctx.Context(), req.Name)
	if err != nil {
		return nil, mapStoreError(err)
	}

	sessions, err := a.eng.Store().ListSessions(ctx.Context(), &session.ListFilter{
		AgentID: cfg.ID,
		Limit:   defaultLimit(req.Limit),
		Offset:  req.Offset,
	})
	if err != nil {
		return nil, mapStoreError(err)
	}

	resp := &ListSessionsResponse{Items: sessions}
	return resp, ctx.JSON(http.StatusOK, resp)
}

func (a *API) countSessions(ctx forge.Context, req *CountSessionsRequest) (*CountSessionsResponse, error) {
	cfg, err := a.eng.GetAgentByName(ctx.Context(), req.Name)
	if err != nil {
		return nil, mapStoreError(err)
	}

	count, err := a.eng.Store().CountSessions(ctx.Context(), &session.ListFilter{
		AgentID: cfg.ID,
	})
	if err != nil {
		return nil, mapStoreError(err)
	}

	resp := &CountSessionsResponse{Count: count}
	return resp, ctx.JSON(http.StatusOK, resp)
}

func (a *API) updateSession(ctx forge.Context, req *UpdateSessionRequest) (*session.Session, error) {
	cfg, err := a.eng.GetAgentByName(ctx.Context(), req.Name)
	if err != nil {
		return nil, mapStoreError(err)
	}

	sessionID, err := id.ParseSessionID(req.SessionID)
	if err != nil {
		return nil, forge.BadRequest(fmt.Sprintf("invalid session ID: %v", err))
	}

	sess, err := a.resolveOwnedSession(ctx.Context(), cfg.ID, sessionID)
	if err != nil {
		return nil, mapStoreError(err)
	}

	if req.Title != "" {
		sess.Title = req.Title
	}
	if req.Metadata != nil {
		sess.Metadata = req.Metadata
	}

	if err := a.eng.Store().UpdateSession(ctx.Context(), sess); err != nil {
		return nil, mapStoreError(err)
	}

	return sess, ctx.JSON(http.StatusOK, sess)
}

func (a *API) deleteSession(ctx forge.Context, req *DeleteSessionRequest) (*struct{}, error) {
	cfg, err := a.eng.GetAgentByName(ctx.Context(), req.Name)
	if err != nil {
		return nil, mapStoreError(err)
	}

	sessionID, err := id.ParseSessionID(req.SessionID)
	if err != nil {
		return nil, forge.BadRequest(fmt.Sprintf("invalid session ID: %v", err))
	}

	if _, err := a.resolveOwnedSession(ctx.Context(), cfg.ID, sessionID); err != nil {
		return nil, mapStoreError(err)
	}

	if err := a.eng.Store().DeleteSession(ctx.Context(), sessionID); err != nil {
		return nil, mapStoreError(err)
	}

	return nil, ctx.NoContent(http.StatusNoContent)
}

// resolveOwnedSession fetches a session by id and confirms it belongs to
// agentID, folding a mismatch into cortex.ErrSessionNotFound rather than a
// distinct "forbidden" error -- the same shape resolveConversationSession
// in memory_handler.go uses. A session id in this handler's path always
// comes from an agent-scoped URL, so without this check a caller could
// name agent A in the path and pass agent B's session id, reading or
// mutating a session that has nothing to do with the agent named in the
// request.
func (a *API) resolveOwnedSession(ctx context.Context, agentID id.AgentID, sessionID id.SessionID) (*session.Session, error) {
	sess, err := a.eng.Store().GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if sess.AgentID != agentID {
		return nil, cortex.ErrSessionNotFound
	}
	return sess, nil
}
