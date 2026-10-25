package assurance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

const kindReservedReply = "a2a_reserved_reply"

// MessagingTransaction operates only inside its backend's active transaction.
// Reads and writes must constrain both exact scope and routing partition.
type MessagingTransaction interface {
	Driver
	LockConversation(context.Context, id.ConversationID) error
	ReadDelivery(context.Context, id.DeliveryID) (*a2a.Delivery, error)
	ReadMessage(context.Context, id.MessageID) (*a2a.Envelope, error)
	ReadConversation(context.Context, id.ConversationID) (*a2a.Conversation, error)
	SaveMessage(context.Context, *a2a.Envelope, *a2a.Conversation) error
	SaveReplyDelivery(context.Context, *a2a.Delivery) error
	FinishReplyDelivery(context.Context, *a2a.Delivery, time.Time) error
}

// MessagingDriver locks one delivery or conversation before invoking storage
// work. Mongo may replay the callback; it must contain no external side effects.
type MessagingDriver interface {
	TransactMessaging(context.Context, id.DeliveryID, id.ConversationID, func(context.Context, MessagingTransaction) error) error
}

func (s *Repository) messagingTransaction(ctx context.Context, delivery id.DeliveryID, conversation id.ConversationID, fn func(context.Context, MessagingTransaction, *Repository) error) error {
	if _, err := cortex.RequireScope(ctx); err != nil {
		return err
	}
	driver, ok := s.driver.(MessagingDriver)
	if !ok {
		return a2a.ErrReplyCommitUnavailable
	}
	return driver.TransactMessaging(ctx, delivery, conversation, func(tc context.Context, tx MessagingTransaction) error {
		if err := fn(tc, tx, &Repository{driver: tx, clock: s.clock}); err != nil {
			return err
		}
		return tc.Err()
	})
}

func envelopeRequest(e *a2a.Envelope) ([]byte, error) {
	snapshot := *e
	snapshot.Entity, snapshot.Hops = cortex.Entity{}, 0
	return json.Marshal(snapshot)
}

// RecordMessage records an immutable envelope and hop admission atomically.
func (s *Repository) RecordMessage(ctx context.Context, input *a2a.Envelope) (*a2a.Envelope, error) {
	if input == nil || input.ID.IsNil() || input.ID.Prefix() != id.PrefixMessage || input.ConversationID.IsNil() || input.ConversationID.Prefix() != id.PrefixConversation || input.Validate() != nil || !utf8.ValidString(input.Content) {
		return nil, a2a.ErrReplyCommitConflict
	}
	if err := require(ctx, input.Scope); err != nil {
		return nil, err
	}
	data, err := json.Marshal(input)
	if err != nil || len(data) > 5<<20 {
		return nil, a2a.ErrReplyCommitConflict
	}
	var message a2a.Envelope
	if decodeErr := json.Unmarshal(data, &message); decodeErr != nil {
		return nil, decodeErr
	}
	message.OrchestrationID = a2a.RoutingOrchestration(ctx)
	want, err := envelopeRequest(&message)
	if err != nil {
		return nil, err
	}
	var result *a2a.Envelope
	err = s.messagingTransaction(ctx, id.Nil, message.ConversationID, func(tc context.Context, tx MessagingTransaction, _ *Repository) error {
		current, readErr := tx.ReadMessage(tc, message.ID)
		if readErr == nil {
			got, encodeErr := envelopeRequest(current)
			if encodeErr != nil || !bytes.Equal(got, want) {
				return a2a.ErrReplyCommitConflict
			}
			result = current
			return nil
		}
		if !errors.Is(readErr, a2a.ErrMessageNotFound) {
			return readErr
		}
		conv, readErr := tx.ReadConversation(tc, message.ConversationID)
		if readErr != nil {
			return readErr
		}
		candidate := message
		if writeErr := saveAdmittedMessage(tc, tx, &candidate, conv, s.clock.Now()); writeErr != nil {
			return writeErr
		}
		result = &candidate
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func saveAdmittedMessage(ctx context.Context, tx MessagingTransaction, message *a2a.Envelope, conv *a2a.Conversation, now time.Time) error {
	if !conv.IsOpen() {
		return a2a.ErrConversationClosed
	}
	if conv.HopsUsed < 0 || conv.HopsUsed >= conv.HopCeiling {
		return a2a.ErrHopCeiling
	}
	message.Hops = conv.HopsUsed + 1
	message.Entity = cortex.Entity{CreatedAt: now, UpdatedAt: now}
	conv.HopsUsed, conv.UpdatedAt = message.Hops, now
	conv.AddParticipant(message.Sender)
	for _, receiver := range message.Receivers {
		conv.AddParticipant(receiver)
	}
	return tx.SaveMessage(ctx, message, conv)
}

func replyKey(ctx context.Context, key id.DeliveryID) string {
	return a2a.RoutingOrchestration(ctx) + ":" + key.String()
}

// GetReservedReply returns a committed reply observation, never dispatch rights.
func (s *Repository) GetReservedReply(ctx context.Context, key id.DeliveryID) (a2a.ReservedReply, error) {
	var receipt a2a.ReservedReply
	if key.IsNil() || key.Prefix() != id.PrefixDelivery {
		return receipt, a2a.ErrReplyCommitConflict
	}
	row, err := s.get(ctx, Query{Kind: kindReservedReply, ID: replyKey(ctx, key)})
	if errors.Is(err, ErrAbsent) {
		return receipt, a2a.ErrReplyCommitNotFound
	}
	if err != nil {
		return receipt, err
	}
	if row.Deleted || len(row.Body) > 16<<10 || json.Unmarshal([]byte(row.Body), &receipt) != nil || receipt.SchemaVersion != 1 || receipt.DeliveryID != key || receipt.Scope.Canonical() != row.ScopeCanon || receipt.OrchestrationID != a2a.RoutingOrchestration(ctx) || row.Digest != evidence.Digest([]byte(row.Body)) || !evidence.ValidDigest(receipt.RequestDigest) || receipt.RunID.Prefix() != id.PrefixAgentRun || receipt.ReplyMessageID.Prefix() != id.PrefixMessage || receipt.ReplyDeliveryID.Prefix() != id.PrefixDelivery || receipt.CommittedAt.IsZero() || row.RunID != receipt.OrchestrationID || row.ID != replyKey(ctx, key) || row.Kind != kindReservedReply {
		return a2a.ReservedReply{}, a2a.ErrReplyCommitConflict
	}
	return receipt, nil
}

// CommitReservedReply closes a bound delivery and queues its reply in one commit.
func (s *Repository) CommitReservedReply(ctx context.Context, input *a2a.Delivery, content string, success bool) (a2a.ReservedReply, error) {
	var result a2a.ReservedReply
	if input == nil || input.ID.Prefix() != id.PrefixDelivery || input.RunID.Prefix() != id.PrefixAgentRun || input.ClaimToken == "" || len(content) > 4<<20 || !utf8.ValidString(content) || input.OrchestrationID == "" || input.OrchestrationID != a2a.RoutingOrchestration(ctx) {
		return result, a2a.ErrReplyCommitConflict
	}
	if err := require(ctx, input.Scope); err != nil {
		return result, err
	}
	delivery := *input
	delivery.Scope.Levels = slices.Clone(input.Scope.Levels)
	data, err := json.Marshal(struct {
		ID, RunID, MessageID id.ID
		Claim, Team, Content string
		Receiver             a2a.Address
		Success              bool
	}{delivery.ID, delivery.RunID, delivery.MessageID, delivery.ClaimToken, delivery.OrchestrationID, content, delivery.Receiver, success})
	if err != nil {
		return result, err
	}
	digest := evidence.Digest(data)
	err = s.messagingTransaction(ctx, delivery.ID, id.Nil, func(tc context.Context, tx MessagingTransaction, repo *Repository) error {
		prior, priorErr := repo.GetReservedReply(tc, delivery.ID)
		if priorErr == nil {
			if prior.RequestDigest != digest {
				return a2a.ErrReplyCommitConflict
			}
			result = prior
			return nil
		}
		if !errors.Is(priorErr, a2a.ErrReplyCommitNotFound) {
			return priorErr
		}
		current, readErr := tx.ReadDelivery(tc, delivery.ID)
		if readErr != nil {
			return readErr
		}
		if current.State != a2a.DeliveryDelivering || current.RunID != delivery.RunID || current.ClaimToken != delivery.ClaimToken || current.MessageID != delivery.MessageID || current.Receiver != delivery.Receiver {
			return a2a.ErrDeliveryClaimLost
		}
		original, readErr := tx.ReadMessage(tc, current.MessageID)
		if readErr != nil {
			return readErr
		}
		class, valid := original.Performative.Class()
		if !valid || class != a2a.ClassDirective || !slices.Contains(original.Receivers, current.Receiver) {
			return a2a.ErrReplyCommitConflict
		}
		if lockErr := tx.LockConversation(tc, original.ConversationID); lockErr != nil {
			return lockErr
		}
		conv, readErr := tx.ReadConversation(tc, original.ConversationID)
		if readErr != nil {
			return readErr
		}
		at := s.clock.Now()
		reply := &a2a.Envelope{ID: id.NewMessageID(), Scope: delivery.Scope, OrchestrationID: delivery.OrchestrationID, Sender: current.Receiver, Receivers: []a2a.Address{original.Sender}, Performative: a2a.Inform, Content: content, ConversationID: original.ConversationID, InReplyTo: original.ReplyWith, Protocol: original.Protocol, Ontology: original.Ontology, OriginRunID: current.RunID}
		if !success {
			reply.Performative = a2a.Failure
		}
		if reply.Validate() != nil {
			return a2a.ErrReplyCommitConflict
		}
		if saveErr := saveAdmittedMessage(tc, tx, reply, conv, at); saveErr != nil {
			return saveErr
		}
		queued := &a2a.Delivery{Entity: cortex.Entity{CreatedAt: at, UpdatedAt: at}, ID: id.NewDeliveryID(), Scope: delivery.Scope, OrchestrationID: delivery.OrchestrationID, MessageID: reply.ID, Receiver: original.Sender, State: a2a.DeliveryQueued}
		if saveErr := tx.SaveReplyDelivery(tc, queued); saveErr != nil {
			return saveErr
		}
		if finishErr := tx.FinishReplyDelivery(tc, current, at); finishErr != nil {
			return finishErr
		}
		result = a2a.ReservedReply{SchemaVersion: 1, Scope: delivery.Scope, OrchestrationID: delivery.OrchestrationID, DeliveryID: delivery.ID, RunID: delivery.RunID, ReplyMessageID: reply.ID, ReplyDeliveryID: queued.ID, RequestDigest: digest, CommittedAt: at}
		body, encodeErr := json.Marshal(result)
		if encodeErr != nil {
			return encodeErr
		}
		row, recordErr := record(kindReservedReply, replyKey(tc, delivery.ID), evidence.Digest(body), delivery.OrchestrationID, delivery.Scope, result)
		if recordErr != nil {
			return recordErr
		}
		if insertErr := repo.insert(tc, row); insertErr != nil {
			return insertErr
		}
		result.Fresh = true
		return nil
	})
	if err != nil {
		return a2a.ReservedReply{}, err
	}
	return result, nil
}
