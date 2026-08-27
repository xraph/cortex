package a2a

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
)

// InboxFilter controls an inbox listing. Scope arrives on the context.
type InboxFilter struct {
	UnreadOnly     bool
	ConversationID id.ConversationID
	Limit          int
	Offset         int
}

// MessageListFilter controls a message listing. Scope arrives on the
// context; Exact narrows to rows stored at precisely that depth instead of
// everything beneath it.
type MessageListFilter struct {
	Exact          bool
	ConversationID id.ConversationID
	Limit          int
	Offset         int
}

// ConversationListFilter controls a conversation listing.
type ConversationListFilter struct {
	Exact  bool
	Status string
	Limit  int
	Offset int
}

// Store is persistence for the messaging subsystem. It folds into the
// composite store.Store the same way orchestration's two interfaces do.
type Store interface {
	CreateMessage(ctx context.Context, e *Envelope) error
	GetMessage(ctx context.Context, msgID id.MessageID) (*Envelope, error)
	ListMessages(ctx context.Context, filter *MessageListFilter) ([]*Envelope, error)

	CreateConversation(ctx context.Context, c *Conversation) error
	GetConversation(ctx context.Context, convID id.ConversationID) (*Conversation, error)
	UpdateConversation(ctx context.Context, c *Conversation) error
	ListConversations(ctx context.Context, filter *ConversationListFilter) ([]*Conversation, error)

	CreateDelivery(ctx context.Context, d *Delivery) error
	UpdateDelivery(ctx context.Context, d *Delivery) error
	// ClaimDelivery takes ownership of a queued delivery and marks it
	// delivering. It returns ErrDeliveryAlreadyClaimed when the row is in
	// any other state, which is what stops two workers running one
	// directive twice.
	ClaimDelivery(ctx context.Context, deliveryID id.DeliveryID) (*Delivery, error)
	ListInbox(ctx context.Context, agentName string, filter InboxFilter) ([]*Delivery, error)
	ListQueuedDeliveries(ctx context.Context, limit int) ([]*Delivery, error)
	MarkDeliveryRead(ctx context.Context, deliveryID id.DeliveryID) error

	CreatePendingAsk(ctx context.Context, a *PendingAsk) error
	// ClaimPendingAsk takes ownership of the ask carrying replyWith. It
	// returns ErrAskNotFound when no such row exists and
	// ErrAskAlreadyClaimed when another caller got there first. Claiming
	// before resuming is what keeps a run from being resumed twice.
	ClaimPendingAsk(ctx context.Context, replyWith string) (*PendingAsk, error)
	ListExpiredAsks(ctx context.Context, now time.Time, limit int) ([]*PendingAsk, error)
}
