package a2a

import (
	"context"
	"fmt"

	"github.com/xraph/cortex/id"
)

// deliverOne carries one queued delivery to its receiver. What that means
// depends on the routing class, and the three cases below are the whole of
// the delivery contract.
//
// A recipient's failure is never returned here. It becomes a failure
// message the sender can read, because a peer that broke is information for
// the asker and not a reason to stop the delivery loop.
func (b *Bus) deliverOne(ctx context.Context, deliveryID id.DeliveryID) error {
	d, err := b.store.ClaimDelivery(ctx, deliveryID)
	if err != nil {
		return err
	}
	e, err := b.store.GetMessage(ctx, d.MessageID)
	if err != nil {
		return b.failDelivery(ctx, d, err)
	}

	class, ok := e.Performative.Class()
	if !ok {
		return b.failDelivery(ctx, d, ErrInvalidPerformative)
	}

	switch class {
	case ClassInformative:
		return b.finishDelivery(ctx, d, e, id.AgentRunID{})
	case ClassControl:
		if err := b.handleControl(ctx, e); err != nil {
			return b.failDelivery(ctx, d, err)
		}
		return b.finishDelivery(ctx, d, e, id.AgentRunID{})
	case ClassDirective:
		return b.runDirective(ctx, d, e)
	default:
		return b.failDelivery(ctx, d, ErrInvalidPerformative)
	}
}

// runDirective starts the recipient's run and turns its outcome into a
// reply on the same conversation.
func (b *Bus) runDirective(ctx context.Context, d *Delivery, e *Envelope) error {
	out, runErr := b.runner.RunAgent(ctx, d.Receiver.Agent, RenderInput(e), nil)

	reply := SendParams{
		Sender:         d.Receiver,
		Receivers:      []Address{e.Sender},
		ConversationID: e.ConversationID,
		InReplyTo:      e.ReplyWith,
		Protocol:       e.Protocol,
		Ontology:       e.Ontology,
	}
	var runID id.AgentRunID
	if runErr != nil {
		reply.Performative = Failure
		reply.Content = fmt.Sprintf("%s could not answer: %v", d.Receiver.Agent, runErr)
	} else {
		reply.Performative = Inform
		reply.Content = out.Output
		runID = out.RunID
	}

	if err := b.finishDelivery(ctx, d, e, runID); err != nil {
		return err
	}
	// A reply that cannot be sent, because the conversation closed or the
	// hop budget ran out, still has to reach whoever is waiting on it.
	if _, err := b.Send(ctx, reply); err != nil {
		return b.resolveAskWithFailure(ctx, e.ReplyWith, err.Error())
	}
	return nil
}

func (b *Bus) finishDelivery(ctx context.Context, d *Delivery, e *Envelope, runID id.AgentRunID) error {
	now := b.clock.Now()
	d.State = DeliveryDelivered
	d.DeliveredAt = &now
	d.RunID = runID
	if err := b.store.UpdateDelivery(ctx, d); err != nil {
		return err
	}
	b.hooks.MessageDelivered(ctx, e.ID, d.Receiver.String())
	return nil
}

func (b *Bus) failDelivery(ctx context.Context, d *Delivery, cause error) error {
	d.State = DeliveryFailed
	d.Error = cause.Error()
	if err := b.store.UpdateDelivery(ctx, d); err != nil {
		return err
	}
	b.hooks.MessageRefused(ctx, d.MessageID, d.Receiver.String(), cause.Error())
	return nil
}

// handleControl interprets a control message. Task 11 fills it in.
func (b *Bus) handleControl(context.Context, *Envelope) error { return nil }

// resolveAskWithFailure un-pauses a waiting ask. Task 10 fills it in.
func (b *Bus) resolveAskWithFailure(context.Context, string, string) error { return nil }
