package a2a

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
)

// The FIPA interaction protocol names, as they travel on Envelope.Protocol.
//
// Stamping one lets a reader of a stored conversation tell a tender from
// an ordinary exchange, and gives a remote peer the name the FIPA
// specification uses rather than one cortex made up.
const (
	ProtocolContractNet = "fipa-contract-net"
	ProtocolRequest     = "fipa-request"
	ProtocolQuery       = "fipa-query"
)

// Proposal is one participant's answer to a call for proposals.
type Proposal struct {
	From    Address `json:"from"`
	Content string  `json:"content"`
}

// Tender is the state of one call for proposals.
type Tender struct {
	ConversationID id.ConversationID `json:"conversation_id"`
	ReplyWith      string            `json:"reply_with"`
	Proposals      []Proposal        `json:"proposals"`
	Refusals       []Proposal        `json:"refusals"`
	// Complete says every agent the call went to has answered.
	Complete bool `json:"complete"`
}

// ContractNetParams is one call for proposals.
type ContractNetParams struct {
	Initiator  Address
	Recipients []Address
	Content    string
	Ontology   string
	ReplyBy    *time.Time
	AskerRunID id.AgentRunID
	ToolCallID string
}

// ContractNet announces a task to several agents at once.
//
// It sends the call and returns the handle to collect against; it does
// not wait, and it does not choose. Waiting is what the asking run's own
// suspension does, and choosing is a judgement this package has no
// business making. An initiator picks its contractor and then awards with
// accept-proposal, which is an ordinary directive.
//
// This is a convenience for hosts driving a tender in Go. An agent needs
// nothing from here: a call for proposals is agent_ask with several
// recipients and the cfp performative, which is the whole point of
// building the protocol out of the primitives rather than beside them.
func ContractNet(ctx context.Context, b *Bus, p ContractNetParams) (*Tender, error) {
	res, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender:       p.Initiator,
			Receivers:    p.Recipients,
			Performative: CFP,
			Content:      p.Content,
			Ontology:     p.Ontology,
			Protocol:     ProtocolContractNet,
			ReplyBy:      p.ReplyBy,
		},
		AskerRunID: p.AskerRunID,
		ToolCallID: p.ToolCallID,
	})
	if err != nil {
		return nil, err
	}
	return &Tender{ConversationID: res.ConversationID, ReplyWith: res.ReplyWith}, nil
}

// CollectTender reads what a call for proposals has gathered so far,
// separating the bids from the declines.
//
// It reads the conversation rather than any state of its own, so it is
// safe to call at any point and after a restart.
func CollectTender(ctx context.Context, b *Bus, convID id.ConversationID, replyWith string) (*Tender, error) {
	msgs, err := b.store.ListMessages(ctx, &MessageListFilter{ConversationID: convID})
	if err != nil {
		return nil, err
	}

	tender := &Tender{ConversationID: convID, ReplyWith: replyWith}
	var expected int
	seen := make(map[string]bool, len(msgs))

	for _, m := range msgs {
		if m.ReplyWith == replyWith {
			expected = len(m.Receivers)
			continue
		}
		if m.InReplyTo != replyWith || seen[m.Sender.String()] {
			continue
		}
		switch m.Performative {
		case Propose:
			seen[m.Sender.String()] = true
			tender.Proposals = append(tender.Proposals, Proposal{From: m.Sender, Content: m.Content})
		case Refuse:
			seen[m.Sender.String()] = true
			tender.Refusals = append(tender.Refusals, Proposal{From: m.Sender, Content: m.Content})
		}
	}

	tender.Complete = expected > 0 && len(seen) >= expected
	return tender, nil
}
