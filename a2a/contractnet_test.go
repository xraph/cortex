package a2a

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
)

// firstReply is the one-answer case every single-recipient test reads.
func firstReply(t *testing.T, payload AskReply) AskReplyItem {
	t.Helper()
	if len(payload.Replies) == 0 {
		t.Fatalf("the payload carries no replies: %+v", payload)
	}
	return payload.Replies[0]
}

// lastReplyContains looks for text in any answer, which is what a
// timeout or a failure test wants: the reason is appended after whatever
// had already arrived.
func lastReplyContains(payload AskReply, text string) bool {
	for _, r := range payload.Replies {
		if strings.Contains(r.Content, text) {
			return true
		}
	}
	return false
}

func decodeAskReply(t *testing.T, raw string) AskReply {
	t.Helper()
	var payload AskReply
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("the resume result must be JSON the tool can return: %v", err)
	}
	return payload
}

// A call for proposals goes to several agents at once, which is exactly
// what the one-receiver rule used to forbid.
func TestAskAcceptsSeveralReceivers(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	res, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender:       Address{Agent: "initiator"},
			Receivers:    []Address{{Agent: "w1"}, {Agent: "w2"}, {Agent: "w3"}},
			Performative: CFP,
			Content:      "who can review this migration?",
			Protocol:     ProtocolContractNet,
		},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	msg, err := st.GetMessage(ctx, res.MessageID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if len(msg.Receivers) != 3 {
		t.Fatalf("the cfp reached %d agents, want 3", len(msg.Receivers))
	}
	if msg.Protocol != ProtocolContractNet {
		t.Errorf("protocol = %q, want the FIPA name so a reader can tell a tender from a chat", msg.Protocol)
	}
}

// The initiator waits for the whole field, not the first one back.
func TestAskWithSeveralReceiversResolvesOnTheLastAnswer(t *testing.T) {
	b, _, _, resumer, _, _ := newTestBus(t)
	ctx := testCtx()

	ask, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender: Address{Agent: "initiator"}, Receivers: []Address{{Agent: "w1"}, {Agent: "w2"}},
			Performative: CFP, Content: "who can take this?", Protocol: ProtocolContractNet,
		},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	reply := func(from, content string, p Performative) {
		t.Helper()
		if _, sendErr := b.Send(ctx, SendParams{
			Sender: Address{Agent: from}, Receivers: []Address{{Agent: "initiator"}},
			Performative: p, Content: content,
			ConversationID: ask.ConversationID, InReplyTo: ask.ReplyWith,
		}); sendErr != nil {
			t.Fatalf("reply from %s: %v", from, sendErr)
		}
	}

	reply("w1", "I can, by 5pm", Propose)
	if resumer.count() != 0 {
		t.Fatal("the initiator resumed before the field had answered")
	}

	reply("w2", "I can, by 3pm", Propose)
	if resumer.count() != 1 {
		t.Fatalf("resumed %d times, want 1 once everyone answered", resumer.count())
	}

	payload := decodeAskReply(t, resumer.last().Result)
	if !payload.Complete {
		t.Error("every recipient answered, so the tender is complete")
	}
	if len(payload.Replies) != 2 {
		t.Fatalf("got %d replies, want both proposals", len(payload.Replies))
	}
}

// One participant declining must not un-pause an initiator that is still
// waiting on the others.
func TestRefuseDoesNotResolveAMultiRecipientAsk(t *testing.T) {
	b, _, _, resumer, _, _ := newTestBus(t)
	ctx := testCtx()

	ask, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender: Address{Agent: "initiator"}, Receivers: []Address{{Agent: "w1"}, {Agent: "w2"}},
			Performative: CFP, Content: "anyone?", Protocol: ProtocolContractNet,
		},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if _, refuseErr := b.Send(ctx, SendParams{
		Sender: Address{Agent: "w1"}, Receivers: []Address{{Agent: "initiator"}},
		Performative: Refuse, Content: "too busy",
		ConversationID: ask.ConversationID, InReplyTo: ask.ReplyWith,
	}); refuseErr != nil {
		t.Fatalf("refusal: %v", refuseErr)
	}
	if resumer.count() != 0 {
		t.Fatal("one refusal ended a tender the others were still answering")
	}

	if _, proposeErr := b.Send(ctx, SendParams{
		Sender: Address{Agent: "w2"}, Receivers: []Address{{Agent: "initiator"}},
		Performative: Propose, Content: "I can do it",
		ConversationID: ask.ConversationID, InReplyTo: ask.ReplyWith,
	}); proposeErr != nil {
		t.Fatalf("proposal: %v", proposeErr)
	}
	if resumer.count() != 1 {
		t.Fatalf("resumed %d times, want 1", resumer.count())
	}

	payload := decodeAskReply(t, resumer.last().Result)
	if len(payload.Replies) != 2 {
		t.Fatalf("got %d replies, want the refusal and the proposal", len(payload.Replies))
	}
	var sawRefusal bool
	for _, r := range payload.Replies {
		if r.Performative == string(Refuse) {
			sawRefusal = true
		}
	}
	if !sawRefusal {
		t.Error("a refusal is an answer and belongs in the result the initiator reads")
	}
}

// A participant that never answers is ordinary in a tender. The deadline
// resolves with whoever did.
func TestAMultiRecipientAskResolvesOnItsDeadlineWithPartialAnswers(t *testing.T) {
	b, _, _, resumer, _, clk := newTestBus(t)
	ctx := testCtx()

	ask, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender: Address{Agent: "initiator"}, Receivers: []Address{{Agent: "w1"}, {Agent: "w2"}, {Agent: "w3"}},
			Performative: CFP, Content: "anyone?", Protocol: ProtocolContractNet,
		},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if _, proposeErr := b.Send(ctx, SendParams{
		Sender: Address{Agent: "w1"}, Receivers: []Address{{Agent: "initiator"}},
		Performative: Propose, Content: "I can",
		ConversationID: ask.ConversationID, InReplyTo: ask.ReplyWith,
	}); proposeErr != nil {
		t.Fatalf("proposal: %v", proposeErr)
	}

	clk.advance(DefaultReplyBy + time.Minute)
	n, err := b.SweepExpiredAsks(ctx)
	if err != nil {
		t.Fatalf("SweepExpiredAsks: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want the overdue tender", n)
	}

	payload := decodeAskReply(t, resumer.last().Result)
	if payload.Complete {
		t.Error("two of three never answered, so the tender is not complete")
	}
	// Whoever answered comes back, and the reason is appended after them.
	// An initiator that heard from one of three should get that one
	// rather than an empty result and a sentence.
	var sawProposal bool
	for _, r := range payload.Replies {
		if r.Content == "I can" && r.Performative == string(Propose) {
			sawProposal = true
		}
	}
	if !sawProposal {
		t.Fatalf("the initiator must get whoever did answer: %+v", payload.Replies)
	}
	if !lastReplyContains(payload, "deadline") {
		t.Fatalf("the initiator must be told why it stopped waiting: %+v", payload.Replies)
	}
}

// The single-recipient case still works, and carries one entry rather
// than changing shape.
func TestASingleRecipientAskCarriesOneReply(t *testing.T) {
	b, st, runner, resumer, _, _ := newTestBus(t)
	ctx := testCtx()
	runner.setOutput("w1", "all clear")

	if _, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "status?"},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	if err := b.deliverOne(ctx, queued[0].ID); err != nil {
		t.Fatalf("deliverOne: %v", err)
	}

	if resumer.count() != 1 {
		t.Fatalf("resumed %d times, want 1", resumer.count())
	}
	payload := decodeAskReply(t, resumer.last().Result)
	if !payload.Complete || len(payload.Replies) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.Replies[0].Content != "all clear" {
		t.Fatalf("content = %q", payload.Replies[0].Content)
	}
}

func TestAskStillNeedsAtLeastOneReceiver(t *testing.T) {
	b, _, _, _, _, _ := newTestBus(t)
	_, err := b.Ask(testCtx(), AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Content: "?"},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "c1",
	})
	if !errorsIs(err, ErrAskNeedsReceivers) {
		t.Fatalf("err = %v, want ErrAskNeedsReceivers", err)
	}
}

// The Go helper is for hosts driving a tender themselves. It collects and
// reports; it does not choose, because choosing is a judgement.
func TestContractNetHelperSeparatesProposalsFromRefusals(t *testing.T) {
	b, _, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	tender, err := ContractNet(ctx, b, ContractNetParams{
		Initiator:  Address{Agent: "initiator"},
		Recipients: []Address{{Agent: "w1"}, {Agent: "w2"}},
		Content:    "who can take this?",
		AskerRunID: id.NewAgentRunID(),
		ToolCallID: "call-1",
	})
	if err != nil {
		t.Fatalf("ContractNet: %v", err)
	}
	if tender.ReplyWith == "" || tender.ConversationID.IsNil() {
		t.Fatalf("a tender needs a handle to collect against: %+v", tender)
	}

	for _, r := range []struct {
		from string
		p    Performative
		text string
	}{
		{"w1", Propose, "I can, by 5pm"},
		{"w2", Refuse, "not today"},
	} {
		if _, replyErr := b.Send(ctx, SendParams{
			Sender: Address{Agent: r.from}, Receivers: []Address{{Agent: "initiator"}},
			Performative: r.p, Content: r.text,
			ConversationID: tender.ConversationID, InReplyTo: tender.ReplyWith,
		}); replyErr != nil {
			t.Fatalf("reply from %s: %v", r.from, replyErr)
		}
	}

	collected, err := CollectTender(ctx, b, tender.ConversationID, tender.ReplyWith)
	if err != nil {
		t.Fatalf("CollectTender: %v", err)
	}
	if len(collected.Proposals) != 1 || collected.Proposals[0].From.Agent != "w1" {
		t.Fatalf("proposals = %+v", collected.Proposals)
	}
	if len(collected.Refusals) != 1 || collected.Refusals[0].From.Agent != "w2" {
		t.Fatalf("refusals = %+v", collected.Refusals)
	}
}
