package a2a

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
)

// ErrRecipientObservationUnavailable means no qualified observer was configured.
var ErrRecipientObservationUnavailable = errors.New("cortex: a2a: recipient observation unavailable")

// Recipient observation states describe retained execution, not result assurance.
const (
	RecipientWaiting   = "waiting"
	RecipientSucceeded = "succeeded"
	RecipientFailed    = "failed"
)

// RecipientRequest binds an observation to the original reserved child and team.
type RecipientRequest struct {
	OrchestrationID id.OrchestrationID
	RunID           id.AgentRunID
	AgentName       string
}

// RecipientObservation is a detached, durable child outcome. Successful Output
// must already be approved for publication. Failed and waiting output is ignored.
type RecipientObservation struct {
	RunID     id.AgentRunID
	AgentName string
	State     string
	Output    string
}

// RecipientObserver reads a child's durable state without executing or resuming
// it. Validate its scope, captured participant, prepared provenance and current
// host authority. A read or authorization error is not a terminal child failure.
type RecipientObserver interface {
	ObserveRecipient(context.Context, RecipientRequest) (RecipientObservation, error)
}

// RecoverRecipient publishes a terminal result for a bound delivery without
// reserving or executing its child again. The host authorizes this exact team
// and delivery before reads and again after observation. Waiting returns an
// empty receipt. Equal retries return the committed receipt without observing
// the child again. This API is not a model tool or an unauthenticated route.
func (b *Bus) RecoverRecipient(ctx context.Context, key id.DeliveryID, authorize func(context.Context, id.OrchestrationID, id.DeliveryID) error) (ReservedReply, error) {
	if authorize == nil || hostport.IsNil(b.recipientObserver) || b.reserved == nil || b.replyCommitter == nil || b.orchestrationID.IsNil() {
		return ReservedReply{}, ErrRecipientObservationUnavailable
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return ReservedReply{}, err
	}
	if key.Prefix() != id.PrefixDelivery {
		return ReservedReply{}, ErrReplyCommitConflict
	}
	check := func() error {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if rosterErr := b.checkRosterScope(ctx); rosterErr != nil {
			return rosterErr
		}
		return authorize(ctx, b.orchestrationID, key)
	}
	if authErr := check(); authErr != nil {
		return ReservedReply{}, authErr
	}
	d, err := b.store.GetDelivery(ctx, key)
	if err != nil {
		return ReservedReply{}, err
	}
	if d == nil || d.ID != key || d.Scope.Canonical() != scope.Canonical() || d.OrchestrationID != b.orchestrationID.String() || d.RunID.Prefix() != id.PrefixAgentRun || d.ClaimToken == "" || !d.Receiver.IsLocal() {
		return ReservedReply{}, ErrReplyCommitConflict
	}
	if rosterErr := b.checkRosterAddress(ctx, d.Receiver); rosterErr != nil {
		return ReservedReply{}, rosterErr
	}
	prior, priorErr := b.replyCommitter.GetReservedReply(ctx, key)
	if priorErr == nil {
		if authErr := check(); authErr != nil {
			return ReservedReply{}, authErr
		}
		if (d.State != DeliveryDelivered && d.State != DeliveryDelivering) || validateReservedReceipt(prior, d) != nil {
			return ReservedReply{}, ErrReplyCommitConflict
		}
		prior.Fresh = false
		b.dispatch.enqueue(prior.ReplyDeliveryID)
		return prior, nil
	}
	if !errors.Is(priorErr, ErrReplyCommitNotFound) {
		return ReservedReply{}, priorErr
	}
	if d.State != DeliveryDelivering {
		return ReservedReply{}, ErrDeliveryClaimLost
	}
	e, err := b.store.GetMessage(ctx, d.MessageID)
	if err != nil {
		return ReservedReply{}, err
	}
	if e == nil || e.ID != d.MessageID || e.Scope.Canonical() != scope.Canonical() || e.OrchestrationID != d.OrchestrationID || !slices.Contains(e.Receivers, d.Receiver) {
		return ReservedReply{}, ErrReplyCommitConflict
	}
	if rosterErr := b.checkRosterEnvelope(ctx, e); rosterErr != nil {
		return ReservedReply{}, rosterErr
	}
	class, valid := e.Performative.Class()
	if !valid || class != ClassDirective || IsHandoffProtocol(e.Protocol, e.Language) {
		return ReservedReply{}, ErrReplyCommitConflict
	}
	if authErr := check(); authErr != nil {
		return ReservedReply{}, authErr
	}
	result, err := b.recipientObserver.ObserveRecipient(ctx, RecipientRequest{OrchestrationID: b.orchestrationID, RunID: d.RunID, AgentName: d.Receiver.Agent})
	if err != nil {
		return ReservedReply{}, err
	}
	if authErr := check(); authErr != nil {
		return ReservedReply{}, authErr
	}
	if result.RunID != d.RunID || result.AgentName != d.Receiver.Agent {
		return ReservedReply{}, ErrReplyCommitConflict
	}
	switch result.State {
	case RecipientWaiting:
		return ReservedReply{}, nil
	case RecipientFailed:
		return b.commitReservedOutcome(ctx, d, e, recipientFailureContent(d.Receiver.Agent), false)
	case RecipientSucceeded:
		if len(result.Output) > 4<<20 || !utf8.ValidString(result.Output) {
			return ReservedReply{}, ErrReplyCommitConflict
		}
		return b.commitReservedOutcome(ctx, d, e, result.Output, true)
	default:
		return ReservedReply{}, ErrReplyCommitConflict
	}
}

func recipientFailureContent(name string) string {
	return fmt.Sprintf("%s could not answer: %v", name, cortex.ErrRunIncomplete)
}

func validateReservedReceipt(receipt ReservedReply, d *Delivery) error {
	if receipt.SchemaVersion != 1 || receipt.Scope.Canonical() != d.Scope.Canonical() || receipt.OrchestrationID != d.OrchestrationID || receipt.DeliveryID != d.ID || receipt.RunID != d.RunID || receipt.ReplyMessageID.Prefix() != id.PrefixMessage || receipt.ReplyDeliveryID.Prefix() != id.PrefixDelivery || receipt.CommittedAt.IsZero() {
		return ErrReplyCommitConflict
	}
	return nil
}

func (b *Bus) commitReservedOutcome(ctx context.Context, d *Delivery, e *Envelope, content string, success bool) (ReservedReply, error) {
	committed, err := b.replyCommitter.CommitReservedReply(ctx, d, content, success)
	if err != nil {
		return ReservedReply{}, err
	}
	if bindingErr := validateReservedReceipt(committed, d); bindingErr != nil {
		return ReservedReply{}, bindingErr
	}
	d.State, d.DeliveredAt = DeliveryDelivered, &committed.CommittedAt
	if committed.Fresh {
		performative := Failure
		if success {
			performative = Inform
		}
		b.hooks.MessageDelivered(ctx, e.ID, d.Receiver.String())
		b.hooks.MessageSent(ctx, committed.ReplyMessageID, d.Receiver.String(), e.Sender.String(), string(performative))
	}
	b.dispatch.enqueue(committed.ReplyDeliveryID)
	return committed, nil
}
