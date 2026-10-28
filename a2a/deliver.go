package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// deliverOne carries one queued delivery to its receiver. What that means
// depends on the routing class, and the three cases below are the whole of
// the delivery contract.
//
// A recipient's failure is never returned here. It becomes a failure
// message the sender can read, because a peer that broke is information for
// the asker and not a reason to stop the delivery loop.
//
// A claimed delivery is one root deliver agent message span; a claim that
// loses its race starts nothing.
func (b *Bus) deliverOne(ctx context.Context, deliveryID id.DeliveryID) error {
	d, err := b.store.ClaimDelivery(ctx, deliveryID)
	if err != nil {
		return err
	}
	// The link is taken by the claim's winner only. Two workers here can
	// list the same row, and the loser must not walk off with it.
	ctx, tr := b.startDelivery(ctx, d, b.dispatch.links.take(deliveryID))
	err = b.carry(ctx, d, tr)
	if isStopping(err) {
		// Nothing ran and nothing was answered. Put the row back so another
		// worker carries it on its next pass, instead of leaving it claimed
		// until the claim goes stale.
		err = errors.Join(err, b.requeueDelivery(ctx, d))
	}
	tr.end(d, err)
	return err
}

// carry is the delivery itself, once claimed.
func (b *Bus) carry(ctx context.Context, d *Delivery, tr *deliveryTrace) error {
	e, err := b.store.GetMessage(ctx, d.MessageID)
	if err != nil {
		return b.failDelivery(ctx, d, err)
	}
	if rosterErr := b.checkRosterEnvelope(ctx, e); rosterErr != nil {
		return b.failDelivery(ctx, d, rosterErr)
	}
	if b.roster != nil && (d.Scope.Canonical() != b.roster.scope || !slices.Contains(e.Receivers, d.Receiver)) {
		return b.failDelivery(ctx, d, ErrRosterDenied)
	}
	tr.envelope(e)
	if IsHandoffProtocol(e.Protocol, e.Language) {
		if handoffErr := b.handoff(ctx, e); handoffErr != nil {
			return b.failDelivery(ctx, d, handoffErr)
		}
		return b.carryHandoff(ctx, d, e)
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
		resumed, resolveErr := b.resolveAsk(ctx, e)
		if resolveErr != nil {
			return resolveErr
		}
		if resumed {
			at := b.clock.Now()
			d.ReadAt = &at
		}
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
	var out *cortex.AgentResult
	var runErr error
	if b.reserved != nil {
		var admissionErr error
		out, runErr, admissionErr = b.reserveDirective(ctx, d, e)
		if admissionErr != nil {
			return admissionErr
		}
	} else {
		out, runErr = b.runner.RunAgent(ctx, d.Receiver.Agent, RenderInput(e), nil)
		if out == nil && isStopping(runErr) {
			// The host refused to start the run. That is no answer from
			// the recipient, so no Failure goes to the asker; deliverOne
			// requeues the delivery for another worker.
			return runErr
		}
	}
	if out == nil && runErr == nil {
		runErr = cortex.ErrRunIncomplete
	}
	if b.reserved != nil && cortex.IsOrchestrationWaiting(runErr) {
		// The retained child still owns this delivery. A pause is neither an
		// answer nor proof of failure, and its binding prevents redispatch.
		return nil
	}

	reply := SendParams{
		Sender:         d.Receiver,
		Receivers:      []Address{e.Sender},
		ConversationID: e.ConversationID,
		InReplyTo:      e.ReplyWith,
		Protocol:       e.Protocol,
		Ontology:       e.Ontology,
	}
	runID := d.RunID
	if out != nil && b.reserved == nil {
		runID = out.RunID
	}
	if runErr != nil {
		reply.Performative = Failure
		if b.reserved != nil {
			reply.Content = recipientFailureContent(d.Receiver.Agent)
		} else {
			reply.Content = fmt.Sprintf("%s could not answer: %v", d.Receiver.Agent, runErr)
		}
	} else {
		reply.Performative = Inform
		reply.Content = out.Output
	}
	if b.replyCommitter != nil {
		_, commitErr := b.commitReservedOutcome(ctx, d, e, reply.Content, runErr == nil)
		return commitErr
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

// collected reads the answers an ask already has, so a failure or a
// timeout hands back what arrived rather than throwing it away.
func (b *Bus) collected(ctx context.Context, ask *PendingAsk) ([]AskReplyItem, error) {
	_, replies, err := b.answersForAsk(ctx, ask)
	return replies, err
}

// resolveAskWithFailure un-pauses a waiting ask with a failure the asking
// agent can read: a timeout, a cancelled conversation, a reply that could
// not be sent. It is the sweep's and the cancel path's way in.
func (b *Bus) resolveAskWithFailure(ctx context.Context, replyWith, reason string) error {
	if replyWith == "" {
		return nil
	}
	// Admitted before the claim or seal, for the same reason as resolveAsk.
	admitted, release, admitErr := b.admit(ctx)
	if admitErr != nil {
		return admitErr
	}
	defer release()
	ctx = admitted
	if b.resolutions != nil {
		return b.resolveSealedFailure(ctx, replyWith, reason)
	}
	ask, err := b.store.ClaimPendingAsk(ctx, replyWith)
	switch {
	case errors.Is(err, ErrAskNotFound), errors.Is(err, ErrAskAlreadyClaimed):
		return nil
	case err != nil:
		return err
	}
	if rosterErr := b.checkRosterAddress(ctx, Address{Agent: ask.AskerAgent}); rosterErr != nil {
		return rosterErr
	}
	if b.resumer == nil {
		return nil
	}
	// A failure carries whatever the ask had collected, plus the reason.
	// An initiator whose tender timed out with two proposals in hand
	// should get those two, not an empty result and a sentence.
	// A read failure here costs the collected answers, not the resume: an
	// asker told nothing is worse than an asker told only the reason.
	replies, collectErr := b.collected(ctx, ask)
	if collectErr != nil {
		replies = nil
	}
	replies = append(replies, AskReplyItem{
		Performative:   string(Failure),
		Sender:         ask.Expected.String(),
		Content:        reason,
		ConversationID: ask.ConversationID.String(),
	})
	payload, err := json.Marshal(AskReply{Replies: replies, Complete: false})
	if err != nil {
		return err
	}
	return b.resumer.ResumeAgentReply(ctx, ask.AskerRunID, ask.ToolCallID, string(payload))
}
