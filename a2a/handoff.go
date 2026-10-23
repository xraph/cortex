package a2a

import (
	"context"
	"strings"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// HandoffProtocol identifies assignment proposals, not ordinary agent prompts.
const HandoffProtocol = "cortex-task-handoff/1"

// HandoffLanguage carries the versioned JSON body through existing text parts.
const HandoffLanguage = "application/vnd.cortex.task-handoff+json;version=1"

// HandoffHandler keeps assignment policy outside the messaging leaf package.
// ValidateHandoff authenticates structure and transport bindings before storage
// and delivery. ReceiveHandoff admits local work with current host authorization
// and durable deduplication. Neither method may turn a proposal into task success.
// Implementations must honor cancellation and retain no mutable envelope aliases.
type HandoffHandler interface {
	ValidateHandoff(context.Context, *Envelope) error
	ReceiveHandoff(context.Context, *Envelope) error
}

// IsHandoffProtocol reserves the namespace, including unsupported versions.
func IsHandoffProtocol(protocol, language string) bool {
	return strings.HasPrefix(protocol, "cortex-task-handoff/") || strings.HasPrefix(language, "application/vnd.cortex.task-handoff")
}
func (b *Bus) handoff(ctx context.Context, e *Envelope) error {
	if !IsHandoffProtocol(e.Protocol, e.Language) {
		return nil
	}
	if e.Protocol != HandoffProtocol || e.Language != HandoffLanguage || e.Encoding != "utf-8" || e.Performative != Propose || len(e.Receivers) != 1 || e.ReplyBy == nil || len(e.ReplyTo) != 0 || e.InReplyTo != "" || e.ReplyWith != "" {
		return cortex.ErrInvalidHandoff
	}
	if b.handoffHandler == nil {
		return cortex.ErrHandoffUnavailable
	}
	return b.handoffHandler.ValidateHandoff(ctx, e)
}
func (b *Bus) carryHandoff(ctx context.Context, d *Delivery, e *Envelope) error {
	conversation, err := b.store.GetConversation(ctx, e.ConversationID)
	if err != nil || conversation == nil || !conversation.IsOpen() {
		return b.failDelivery(ctx, d, ErrConversationClosed)
	}
	if !d.Receiver.Equal(e.Receivers[0]) {
		return b.failDelivery(ctx, d, cortex.ErrInvalidHandoff)
	}
	if !d.Receiver.IsLocal() {
		return b.deliverRemote(ctx, d, e)
	}
	if b.handoffHandler == nil || b.handoffHandler.ReceiveHandoff(ctx, e) != nil {
		return b.failDelivery(ctx, d, cortex.ErrHandoffUnavailable)
	}
	return b.finishDelivery(ctx, d, e, id.AgentRunID{})
}
