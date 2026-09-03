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
						"description": "The agent to send to, or a list of them. " +
							"Address a remote agent as name@node.",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "What you are telling them",
					},
					"performative": map[string]any{
						"type": "string",
						"description": "The FIPA-ACL speech act. Defaults to inform. " +
							"Use inform to tell, confirm/disconfirm to answer a query, " +
							"propose to bid on a cfp, refuse to decline it, " +
							"reject-proposal to turn down a bid you did not pick, " +
							"failure to report that something went wrong, " +
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
			Description: "Ask one or more agents something and wait for the answer. " +
				"Your run pauses until they reply, and their answers come back as this tool's result. " +
				"Ask several at once with a cfp to put work out to tender: you get every proposal and " +
				"refusal back together, and you pick. The wait survives a restart, so use this " +
				"whenever you genuinely need an answer before carrying on.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"to": map[string]any{
						"description": "The agent to ask, or a list of agents to put the question to at once. " +
							"Address a remote agent as name@node.",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "What you are asking them to do or answer",
					},
					"performative": map[string]any{
						"type": "string",
						"description": "The FIPA-ACL speech act. Defaults to request. " +
							"Use request to ask for work, query-if to ask whether something holds, " +
							"query-ref to ask for a value, cfp to invite proposals from several agents, " +
							"accept-proposal to award work to whoever you picked.",
					},
					"protocol": map[string]any{
						"type": "string",
						"description": "Optional interaction protocol name. Use fipa-contract-net when " +
							"you are running a tender: cfp to everyone, then accept-proposal to the winner " +
							"and reject-proposal to the rest.",
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
	To             recipients `json:"to"`
	Content        string     `json:"content"`
	Performative   string     `json:"performative"`
	ConversationID string     `json:"conversation_id"`
	Ontology       string     `json:"ontology"`
	Protocol       string     `json:"protocol"`
}

type agentAskArgs struct {
	To             recipients `json:"to"`
	Content        string     `json:"content"`
	Performative   string     `json:"performative"`
	ConversationID string     `json:"conversation_id"`
	Ontology       string     `json:"ontology"`
	Protocol       string     `json:"protocol"`
}

// recipients accepts either one name or a list of them.
//
// Both spellings exist because both readings are natural: asking one
// agent is a name, and putting work out to tender is a list. Forcing a
// model to wrap a single name in an array is the kind of papercut that
// shows up as a malformed tool call rather than as a complaint.
type recipients []string

// UnmarshalJSON accepts "worker" and ["worker","assistant"] alike.
func (r *recipients) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*r = recipients{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("to must be an agent name or a list of them: %w", err)
	}
	*r = many
	return nil
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
		Protocol:     args.Protocol,
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
	if len(args.To) == 0 || args.Content == "" {
		return jsonResult("error", "to and content are required"), outcomeFailed, true
	}

	sender, err := e.a2aSelf(ctx, inv)
	if err != nil {
		return jsonResult("error", err.Error()), outcomeFailed, true
	}
	params := a2a.AskParams{
		SendParams: a2a.SendParams{
			Sender:       sender,
			Receivers:    addressesOf(args.To),
			Performative: performativeOr(args.Performative, a2a.Request),
			Content:      args.Content,
			Ontology:     args.Ontology,
			Protocol:     args.Protocol,
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

// addressesOf parses what the model wrote. Agents address peers as text,
// and "worker@peer.example" has to reach the remote worker rather than a
// local agent whose name contains an @.
func addressesOf(names []string) []a2a.Address {
	out := make([]a2a.Address, 0, len(names))
	for _, n := range names {
		out = append(out, a2a.ParseAddress(n))
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
