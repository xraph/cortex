package a2a

import (
	"context"
	"errors"
	"slices"

	"github.com/xraph/cortex"
)

// ErrReplyBinding means retained ask identity disagrees with its source message.
var ErrReplyBinding = errors.New("cortex: a2a: invalid reply binding")

// tallyAnswers finds the durable ask before interpreting any correlation token.
// Another message carrying that token cannot replace the original recipients.
func (b *Bus) tallyAnswers(ctx context.Context, latest *Envelope) (*PendingAsk, bool, []AskReplyItem, error) {
	asks, err := b.store.ListPendingAsksByConversation(ctx, latest.ConversationID)
	if err != nil {
		return nil, false, nil, err
	}
	for _, ask := range asks {
		if ask.ReplyWith != latest.InReplyTo {
			continue
		}
		if ask.ConversationID != latest.ConversationID || ask.Scope.Canonical() != latest.Scope.Canonical() {
			return nil, false, nil, ErrReplyBinding
		}
		ready, replies, collectErr := b.answersForAsk(ctx, ask)
		return ask, ready, replies, collectErr
	}
	return nil, false, nil, nil
}

func (b *Bus) answersForAsk(ctx context.Context, ask *PendingAsk) (bool, []AskReplyItem, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return false, nil, err
	}
	if ask == nil || ask.MessageID.IsNil() || ask.Scope.Canonical() != scope.Canonical() {
		return false, nil, ErrReplyBinding
	}
	question, err := b.store.GetMessage(ctx, ask.MessageID)
	if err != nil {
		return false, nil, err
	}
	if rosterErr := b.checkRosterEnvelope(ctx, question); rosterErr != nil {
		return false, nil, rosterErr
	}
	if question == nil || question.ID != ask.MessageID || question.Validate() != nil || question.ConversationID != ask.ConversationID || question.ReplyWith != ask.ReplyWith || question.Sender.Agent != ask.AskerAgent || question.Scope.Canonical() != scope.Canonical() || !question.Receivers[0].Equal(ask.Expected) {
		return false, nil, ErrReplyBinding
	}
	class, valid := question.Performative.Class()
	if !valid || class != ClassDirective {
		return false, nil, ErrReplyBinding
	}
	expected := make(map[Address]bool, len(question.Receivers))
	for _, receiver := range question.Receivers {
		expected[receiver] = true
	}
	msgs, err := b.store.ListMessages(ctx, &MessageListFilter{ConversationID: ask.ConversationID, Exact: true})
	if err != nil {
		return false, nil, err
	}
	replies := make([]AskReplyItem, 0, len(expected))
	answered := make(map[Address]bool, len(expected))
	for _, message := range msgs {
		if message == nil || message.InReplyTo != ask.ReplyWith || message.ConversationID != ask.ConversationID || message.Scope.Canonical() != scope.Canonical() || !message.Performative.AnswersAsk() || !expected[message.Sender] || answered[message.Sender] || !slices.Contains(message.Receivers, question.Sender) {
			continue
		}
		if b.checkRosterEnvelope(ctx, message) != nil {
			continue
		}
		answered[message.Sender] = true
		replies = append(replies, askReplyItem(message))
	}
	return len(replies) == len(expected), replies, nil
}

func sameAskBinding(before, after *PendingAsk) bool {
	return before != nil && after != nil && before.ReplyWith == after.ReplyWith && before.MessageID == after.MessageID && before.ConversationID == after.ConversationID && before.AskerRunID == after.AskerRunID && before.AskerAgent == after.AskerAgent && before.ToolCallID == after.ToolCallID && before.Expected == after.Expected && before.OrchestrationID == after.OrchestrationID && before.Scope.Canonical() == after.Scope.Canonical()
}
