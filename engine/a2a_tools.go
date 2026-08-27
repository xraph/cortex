package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
)

// The three agent-facing messaging tools. They exist only when a host
// configured messaging with WithA2A, the same way knowledge_search exists
// only when a knowledge provider is set.
const (
	toolAgentSend  = "agent_send"
	toolAgentAsk   = "agent_ask"
	toolAgentInbox = "agent_inbox"
)

// a2aTools returns the messaging tool definitions, or nothing when
// messaging is off.
func (e *Engine) a2aTools() []llm.Tool {
	if e.a2a == nil {
		return nil
	}
	return []llm.Tool{
		{
			Name: toolAgentSend,
			Description: "Send a message to one or more other agents and continue working. " +
				"Nothing waits for a reply: use this to inform, confirm, refuse, or hand off. " +
				"Use agent_ask instead when you need an answer before you can carry on.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"to": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Names of the agents to send to",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "What you are telling them",
					},
					"performative": map[string]any{
						"type": "string",
						"description": "The FIPA-ACL speech act. Defaults to inform. " +
							"Use inform to tell, confirm/disconfirm to answer a query, " +
							"refuse to decline, failure to report that something went wrong, " +
							"cancel to end a conversation.",
					},
					"conversation_id": map[string]any{
						"type":        "string",
						"description": "Continue an existing conversation instead of starting one",
					},
					"ontology": map[string]any{
						"type":        "string",
						"description": "Optional subject-matter label the recipient can key on",
					},
				},
				"required": []string{"to", "content"},
			},
		},
		{
			Name: toolAgentAsk,
			Description: "Ask another agent something and wait for the answer. " +
				"Your run pauses until they reply, and their answer comes back as this tool's result. " +
				"The wait survives a restart, so use this whenever you genuinely need their answer.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"to": map[string]any{
						"type":        "string",
						"description": "Name of the agent to ask",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "What you are asking them to do or answer",
					},
					"performative": map[string]any{
						"type": "string",
						"description": "The FIPA-ACL speech act. Defaults to request. " +
							"Use request to ask for work, query-if to ask whether something holds, " +
							"query-ref to ask for a value, cfp to invite proposals, propose to offer one.",
					},
					"conversation_id": map[string]any{
						"type":        "string",
						"description": "Continue an existing conversation instead of starting one",
					},
					"ontology": map[string]any{
						"type":        "string",
						"description": "Optional subject-matter label the recipient can key on",
					},
				},
				"required": []string{"to", "content"},
			},
		},
		{
			Name: toolAgentInbox,
			Description: "Read messages other agents sent you while you were busy. " +
				"Reading marks them read, so each message comes back once.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of messages to read",
					},
					"conversation_id": map[string]any{
						"type":        "string",
						"description": "Only read messages on one conversation",
					},
				},
			},
		},
	}
}

// executeA2ATool runs one messaging tool. The outcome matters as much as
// the result: agent_ask does not complete, it pends, and the loop suspends
// the step around it.
func (e *Engine) executeA2ATool(ctx context.Context, inv cortex.Invocation) (string, toolOutcome, bool) {
	if e.a2a == nil {
		return "", outcomeCompleted, false
	}
	switch inv.Call.Name {
	case toolAgentSend:
		return e.executeAgentSend(ctx, inv), outcomeCompleted, true
	case toolAgentInbox:
		return e.executeAgentInbox(ctx, inv), outcomeCompleted, true
	case toolAgentAsk:
		return e.executeAgentAsk(ctx, inv)
	default:
		return "", outcomeCompleted, false
	}
}

type agentSendArgs struct {
	To             []string `json:"to"`
	Content        string   `json:"content"`
	Performative   string   `json:"performative"`
	ConversationID string   `json:"conversation_id"`
	Ontology       string   `json:"ontology"`
}

type agentAskArgs struct {
	To             string `json:"to"`
	Content        string `json:"content"`
	Performative   string `json:"performative"`
	ConversationID string `json:"conversation_id"`
	Ontology       string `json:"ontology"`
}

type agentInboxArgs struct {
	Limit          int    `json:"limit"`
	ConversationID string `json:"conversation_id"`
}

func (e *Engine) executeAgentSend(ctx context.Context, inv cortex.Invocation) string {
	var args agentSendArgs
	if err := json.Unmarshal([]byte(inv.Call.Arguments), &args); err != nil {
		return jsonResult("error", "invalid arguments: "+err.Error())
	}
	if len(args.To) == 0 || args.Content == "" {
		return jsonResult("error", "to and content are required")
	}

	sender, err := e.a2aSelf(ctx, inv)
	if err != nil {
		return jsonResult("error", err.Error())
	}
	params := a2a.SendParams{
		Sender:       sender,
		Receivers:    addressesOf(args.To),
		Performative: performativeOr(args.Performative, a2a.Inform),
		Content:      args.Content,
		Ontology:     args.Ontology,
		OriginRunID:  inv.RunID,
	}
	if convID, convErr := parseConversationID(args.ConversationID); convErr != nil {
		return jsonResult("error", convErr.Error())
	} else if !convID.IsNil() {
		params.ConversationID = convID
	}

	res, err := e.a2a.Send(ctx, params)
	if err != nil {
		return jsonResult("error", err.Error())
	}
	out, err := json.Marshal(res)
	if err != nil {
		return jsonResult("error", err.Error())
	}
	return string(out)
}

// executeAgentAsk sends the question and reports the call as pending, so
// the step suspends around it. The answer arrives later, as this same
// call's result, when the bus resumes the run.
//
// Everything that can refuse the ask happens before the pend: a run
// suspended on a message that was never sent is a run nothing can resume.
func (e *Engine) executeAgentAsk(ctx context.Context, inv cortex.Invocation) (string, toolOutcome, bool) {
	var args agentAskArgs
	if err := json.Unmarshal([]byte(inv.Call.Arguments), &args); err != nil {
		return jsonResult("error", "invalid arguments: "+err.Error()), outcomeFailed, true
	}
	if args.To == "" || args.Content == "" {
		return jsonResult("error", "to and content are required"), outcomeFailed, true
	}

	sender, err := e.a2aSelf(ctx, inv)
	if err != nil {
		return jsonResult("error", err.Error()), outcomeFailed, true
	}
	params := a2a.AskParams{
		SendParams: a2a.SendParams{
			Sender:       sender,
			Receivers:    []a2a.Address{{Agent: args.To}},
			Performative: performativeOr(args.Performative, a2a.Request),
			Content:      args.Content,
			Ontology:     args.Ontology,
			OriginRunID:  inv.RunID,
		},
		AskerRunID: inv.RunID,
		ToolCallID: inv.Call.ID,
	}
	if convID, convErr := parseConversationID(args.ConversationID); convErr != nil {
		return jsonResult("error", convErr.Error()), outcomeFailed, true
	} else if !convID.IsNil() {
		params.ConversationID = convID
	}

	if _, err := e.a2a.Ask(ctx, params); err != nil {
		// A refused ask goes back to the model as an ordinary tool error.
		// It can pick another agent, or answer without one.
		return jsonResult("error", err.Error()), outcomeFailed, true
	}
	return "", outcomePending, true
}

func (e *Engine) executeAgentInbox(ctx context.Context, inv cortex.Invocation) string {
	var args agentInboxArgs
	if err := json.Unmarshal([]byte(inv.Call.Arguments), &args); err != nil {
		return jsonResult("error", "invalid arguments: "+err.Error())
	}
	self, err := e.a2aSelf(ctx, inv)
	if err != nil {
		return jsonResult("error", err.Error())
	}

	filter := a2a.InboxFilter{UnreadOnly: true, Limit: args.Limit}
	if convID, convErr := parseConversationID(args.ConversationID); convErr != nil {
		return jsonResult("error", convErr.Error())
	} else if !convID.IsNil() {
		filter.ConversationID = convID
	}

	items, err := e.a2a.Inbox(ctx, self.Agent, filter)
	if err != nil {
		return jsonResult("error", err.Error())
	}
	if len(items) == 0 {
		return jsonResult("status", "no new messages")
	}
	out, err := json.Marshal(map[string]any{"messages": items})
	if err != nil {
		return jsonResult("error", err.Error())
	}
	return string(out)
}

// a2aSelf resolves the address of the agent making the call. It reads the
// agent's own name from the store rather than trusting an argument: an
// agent that could name its own sender could impersonate any peer.
func (e *Engine) a2aSelf(ctx context.Context, inv cortex.Invocation) (a2a.Address, error) {
	if e.store == nil {
		return a2a.Address{}, cortex.ErrNoStore
	}
	ag, err := e.store.Get(ctx, inv.AgentID)
	if err != nil {
		return a2a.Address{}, fmt.Errorf("resolve sending agent: %w", err)
	}
	return a2a.Address{Agent: ag.Name}, nil
}

func addressesOf(names []string) []a2a.Address {
	out := make([]a2a.Address, 0, len(names))
	for _, n := range names {
		out = append(out, a2a.Address{Agent: n})
	}
	return out
}

// performativeOr falls back to a default when the model named none, and
// leaves an unrecognised one alone so envelope validation reports it
// rather than this quietly substituting something the agent did not mean.
func performativeOr(named string, fallback a2a.Performative) a2a.Performative {
	if named == "" {
		return fallback
	}
	return a2a.Performative(named)
}

func parseConversationID(s string) (id.ConversationID, error) {
	if s == "" {
		return id.ConversationID{}, nil
	}
	convID, err := id.ParseWithPrefix(s, id.PrefixConversation)
	if err != nil {
		return id.ConversationID{}, fmt.Errorf("invalid conversation_id: %w", err)
	}
	return convID, nil
}
