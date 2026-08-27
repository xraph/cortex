package api

import (
	"fmt"
	"net/http"

	"github.com/xraph/forge"

	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
)

// registerA2ARoutes wires the messaging endpoints. Three of them are
// observability: what conversations exist, what was said, what is waiting
// in an agent's inbox. The fourth is the way in from outside a run, and
// it is the same path a remote peer will terminate into.
func (a *API) registerA2ARoutes(router forge.Router) error {
	g := router.Group("/v1", forge.WithGroupTags("messaging"))

	if err := g.GET("/a2a/conversations", a.listA2AConversations,
		forge.WithSummary("List conversations"),
		forge.WithDescription("Lists agent-to-agent conversations in the caller's scope."),
		forge.WithOperationID("listA2AConversations"),
		forge.WithRequestSchema(ListA2AConversationsRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Conversation list", &ListA2AConversationsResponse{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register a2a routes: %w", err)
	}

	if err := g.GET("/a2a/conversations/:id", a.getA2AConversation,
		forge.WithSummary("Get conversation"),
		forge.WithDescription("Returns one conversation together with its messages, oldest first."),
		forge.WithOperationID("getA2AConversation"),
		forge.WithRequestSchema(GetA2AConversationRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Conversation", &A2AConversationResponse{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register a2a routes: %w", err)
	}

	if err := g.GET("/agents/:name/inbox", a.getAgentInbox,
		forge.WithSummary("Read an agent's inbox"),
		forge.WithDescription("Returns messages delivered to an agent. Reading marks them read."),
		forge.WithOperationID("getAgentInbox"),
		forge.WithRequestSchema(AgentInboxRequest{}),
		forge.WithResponseSchema(http.StatusOK, "Inbox", &AgentInboxResponse{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register a2a routes: %w", err)
	}

	if err := g.POST("/agents/:name/messages", a.sendAgentMessage,
		forge.WithSummary("Send a message to an agent"),
		forge.WithDescription(
			"Sends a message from outside a run: an operator answering an agent, or a host "+
				"injecting work. A reply carrying in_reply_to resumes the run waiting on it, "+
				"which is how a human answers an agent that asked."),
		forge.WithOperationID("sendAgentMessage"),
		forge.WithRequestSchema(SendMessageRequest{}),
		forge.WithCreatedResponse(&a2a.SendResult{}),
		forge.WithErrorResponses(),
	); err != nil {
		return fmt.Errorf("register a2a routes: %w", err)
	}

	return nil
}

func (a *API) listA2AConversations(ctx forge.Context, req *ListA2AConversationsRequest) (*ListA2AConversationsResponse, error) {
	items, err := a.eng.ListConversations(ctx.Context(), &a2a.ConversationListFilter{
		Status: req.Status,
		Limit:  defaultLimit(req.Limit),
		Offset: req.Offset,
	})
	if err != nil {
		return nil, mapStoreError(err)
	}
	resp := &ListA2AConversationsResponse{Items: items}
	return resp, ctx.JSON(http.StatusOK, resp)
}

func (a *API) getA2AConversation(ctx forge.Context, _ *GetA2AConversationRequest) (*A2AConversationResponse, error) {
	convID, err := id.ParseWithPrefix(ctx.Param("id"), id.PrefixConversation)
	if err != nil {
		return nil, forge.BadRequest("invalid conversation id")
	}

	conv, err := a.eng.GetConversation(ctx.Context(), convID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	msgs, err := a.eng.ListMessages(ctx.Context(), &a2a.MessageListFilter{ConversationID: convID})
	if err != nil {
		return nil, mapStoreError(err)
	}

	resp := &A2AConversationResponse{Conversation: conv, Messages: msgs}
	return resp, ctx.JSON(http.StatusOK, resp)
}

func (a *API) getAgentInbox(ctx forge.Context, req *AgentInboxRequest) (*AgentInboxResponse, error) {
	filter := a2a.InboxFilter{UnreadOnly: !req.IncludeRead, Limit: defaultLimit(req.Limit)}
	if req.ConversationID != "" {
		convID, err := id.ParseWithPrefix(req.ConversationID, id.PrefixConversation)
		if err != nil {
			return nil, forge.BadRequest("invalid conversation_id")
		}
		filter.ConversationID = convID
	}

	items, err := a.eng.AgentInbox(ctx.Context(), ctx.Param("name"), filter)
	if err != nil {
		return nil, mapStoreError(err)
	}
	resp := &AgentInboxResponse{Items: items}
	return resp, ctx.JSON(http.StatusOK, resp)
}

func (a *API) sendAgentMessage(ctx forge.Context, req *SendMessageRequest) (*a2a.SendResult, error) {
	if req.From == "" {
		return nil, forge.BadRequest("from is required: a message with no sender cannot be replied to")
	}
	if req.Content == "" {
		return nil, forge.BadRequest("content is required")
	}

	params := a2a.SendParams{
		Sender:       a2a.Address{Agent: req.From},
		Receivers:    []a2a.Address{{Agent: ctx.Param("name")}},
		Performative: a2a.Performative(req.Performative),
		Content:      req.Content,
		Ontology:     req.Ontology,
		InReplyTo:    req.InReplyTo,
	}
	if params.Performative == "" {
		params.Performative = a2a.Inform
	}
	if req.ConversationID != "" {
		convID, err := id.ParseWithPrefix(req.ConversationID, id.PrefixConversation)
		if err != nil {
			return nil, forge.BadRequest("invalid conversation_id")
		}
		params.ConversationID = convID
	}

	res, err := a.eng.SendMessage(ctx.Context(), params)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return res, ctx.JSON(http.StatusCreated, res)
}
