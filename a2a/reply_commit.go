package a2a

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

var (
	// ErrReplyCommitUnavailable means atomic reply publication is unsupported.
	ErrReplyCommitUnavailable = errors.New("cortex: a2a: atomic reply commit unavailable")
	// ErrReplyCommitConflict means a different result already owns this delivery.
	ErrReplyCommitConflict = errors.New("cortex: a2a: reply commit conflict")
	// ErrReplyCommitNotFound means no reply was committed for this delivery.
	ErrReplyCommitNotFound = errors.New("cortex: a2a: reply commit not found")
)

// ReservedReply retains the reply and queue identities committed with a bound
// delivery. Fresh describes this acknowledgment only and is never persisted.
// Reading a receipt grants no permission to execute or resume a child.
type ReservedReply struct {
	SchemaVersion   int           `json:"schema_version"`
	Scope           cortex.Scope  `json:"scope"`
	OrchestrationID string        `json:"orchestration_id"`
	DeliveryID      id.DeliveryID `json:"delivery_id"`
	RunID           id.AgentRunID `json:"run_id"`
	ReplyMessageID  id.MessageID  `json:"reply_message_id"`
	ReplyDeliveryID id.DeliveryID `json:"reply_delivery_id"`
	RequestDigest   string        `json:"request_digest"`
	CommittedAt     time.Time     `json:"committed_at"`
	Fresh           bool          `json:"-"`
}

// AtomicMessageStore serializes hop admission with immutable envelope storage.
// An identical retry returns the original envelope without another hop charge.
type AtomicMessageStore interface {
	RecordMessage(context.Context, *Envelope) (*Envelope, error)
}

// ReservedReplyStore atomically finishes the claimed delivery, records its
// immutable reply, charges a conversation hop and queues the reply delivery.
// It never invokes a provider or resumes a run. Equal retries return the stored
// receipt; changed content or binding fails. All operations require exact scope
// and routing partition. Success describes the child result, not its assurance.
type ReservedReplyStore interface {
	AtomicMessageStore
	CommitReservedReply(context.Context, *Delivery, string, bool) (ReservedReply, error)
	GetReservedReply(context.Context, id.DeliveryID) (ReservedReply, error)
}

func (s *routingStore) RecordMessage(ctx context.Context, e *Envelope) (*Envelope, error) {
	writer, ok := s.inner.(AtomicMessageStore)
	if !ok {
		return nil, ErrReplyCommitUnavailable
	}
	return writer.RecordMessage(s.context(ctx), e)
}

func (s *routingStore) CommitReservedReply(ctx context.Context, d *Delivery, content string, success bool) (ReservedReply, error) {
	writer, ok := s.inner.(ReservedReplyStore)
	if !ok {
		return ReservedReply{}, ErrReplyCommitUnavailable
	}
	return writer.CommitReservedReply(s.context(ctx), d, content, success)
}

func (s *routingStore) GetReservedReply(ctx context.Context, key id.DeliveryID) (ReservedReply, error) {
	reader, ok := s.inner.(ReservedReplyStore)
	if !ok {
		return ReservedReply{}, ErrReplyCommitUnavailable
	}
	return reader.GetReservedReply(s.context(ctx), key)
}
