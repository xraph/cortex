package engine

import (
	"context"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
)

// ErrNoA2A is returned when a caller reaches for messaging on an engine
// that was never configured with WithA2A. It is distinct from ErrNoStore:
// the store may be perfectly fine and messaging simply switched off.
var ErrNoA2A = errors.New("cortex: agent-to-agent messaging is not configured")

// The read passthroughs below mirror the orchestration CRUD ones: the API
// layer talks to the engine, never to the store directly, so a host that
// swaps the store keeps one seam to think about.

// ListConversations returns messaging conversations in the caller's scope.
func (e *Engine) ListConversations(ctx context.Context, filter *a2a.ConversationListFilter) ([]*a2a.Conversation, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListConversations(ctx, filter)
}

// GetConversation returns one conversation in the caller's scope.
func (e *Engine) GetConversation(ctx context.Context, convID id.ConversationID) (*a2a.Conversation, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetConversation(ctx, convID)
}

// ListMessages returns messages, optionally narrowed to one conversation.
func (e *Engine) ListMessages(ctx context.Context, filter *a2a.MessageListFilter) ([]*a2a.Envelope, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListMessages(ctx, filter)
}

// AgentInbox returns an agent's delivered messages.
//
// It goes through the bus rather than the store because reading an inbox
// marks what it returns as read, and that is the bus's rule rather than a
// storage detail.
func (e *Engine) AgentInbox(ctx context.Context, agentName string, filter a2a.InboxFilter) ([]a2a.InboxItem, error) {
	if e.a2a == nil {
		return nil, ErrNoA2A
	}
	return e.a2a.Inbox(ctx, agentName, filter)
}

// SendMessage injects a message from outside a run: an operator answering
// an agent, an HTTP handler carrying one in, or (later) a remote peer.
//
// It is the same path an agent's own agent_send takes, so a message from
// outside is delivered, hop-counted and correlated exactly like one from
// inside. A reply carrying in_reply_to therefore resumes a waiting run
// here too, which is what lets a human answer an agent that asked.
func (e *Engine) SendMessage(ctx context.Context, params a2a.SendParams) (*a2a.SendResult, error) {
	if e.a2a == nil {
		return nil, ErrNoA2A
	}
	return e.a2a.Send(ctx, params)
}
