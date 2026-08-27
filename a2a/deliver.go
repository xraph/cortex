package a2a

import (
	"context"
	"encoding/json"
	"errors"
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

	// A remote receiver is carried by a transport, whatever the
	// performative says. Routing by class first would run a LOCAL agent
	// named like the remote one, which is how a question addressed to
	// somebody else's system ends up answered by yours.
	if !d.Receiver.IsLocal() {
		return b.deliverRemote(ctx, d, e)
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

// deliverRemote hands one delivery to whichever transport claims the
// address. A transport failure is a failed delivery and never a
// fallback: answering another system's question with a local agent of
// the same name would be worse than not answering it.
//
// Nothing waits here for a reply. The transport's job is to get the
// envelope there; the answer arrives later as an ordinary inbound
// message carrying InReplyTo, which is the path a local reply takes too.
func (b *Bus) deliverRemote(ctx context.Context, d *Delivery, e *Envelope) error {
	t := b.transportFor(d.Receiver)
	if t == nil {
		return b.failDelivery(ctx, d, fmt.Errorf("%w: %s", ErrUnroutable, d.Receiver))
	}
	if err := t.Deliver(ctx, e, d.Receiver); err != nil {
		// An unreachable peer is something the asking agent can act on,
		// so it hears about it now rather than at its deadline.
		if askErr := b.resolveAskWithFailure(ctx, e.ReplyWith, err.Error()); askErr != nil {
			return askErr
		}
		return b.failDelivery(ctx, d, err)
	}
	return b.finishDelivery(ctx, d, e, id.AgentRunID{})
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

// handleControl interprets a control message. cancel closes the
// conversation and un-pauses everyone waiting on it: a run paused behind a
// cancelled conversation would otherwise sit until its deadline and learn
// nothing when it got there.
func (b *Bus) handleControl(ctx context.Context, e *Envelope) error {
	if e.Performative != Cancel {
		return nil
	}
	asks, err := b.store.ListPendingAsksByConversation(ctx, e.ConversationID)
	if err != nil {
		return err
	}
	reason := fmt.Sprintf("conversation cancelled by %s: %s", e.Sender, e.Content)
	for _, a := range asks {
		if failErr := b.resolveAskWithFailure(ctx, a.ReplyWith, reason); failErr != nil {
			return failErr
		}
	}

	conv, err := b.store.GetConversation(ctx, e.ConversationID)
	if err != nil {
		return err
	}
	conv.Status = StatusClosed
	return b.store.UpdateConversation(ctx, conv)
}

// resolveAskWithFailure un-pauses a waiting ask with a failure the asking
// agent can read: a timeout, a cancelled conversation, a reply that could
// not be sent. It is the sweep's and the cancel path's way in.
func (b *Bus) resolveAskWithFailure(ctx context.Context, replyWith, reason string) error {
	if replyWith == "" {
		return nil
	}
	ask, err := b.store.ClaimPendingAsk(ctx, replyWith)
	switch {
	case errors.Is(err, ErrAskNotFound), errors.Is(err, ErrAskAlreadyClaimed):
		return nil
	case err != nil:
		return err
	}
	if b.resumer == nil {
		return nil
	}
	payload, err := json.Marshal(AskReply{
		Performative:   string(Failure),
		Sender:         ask.Expected.String(),
		Content:        reason,
		ConversationID: ask.ConversationID.String(),
	})
	if err != nil {
		return err
	}
	return b.resumer.ResumeAgentReply(ctx, ask.AskerRunID, ask.ToolCallID, string(payload))
}
