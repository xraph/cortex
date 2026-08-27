package a2a

import (
	"encoding/json"
	"testing"

	"github.com/xraph/cortex/id"
)

// The full loop: A asks, B answers, A's run resumes with B's words.
func TestReplyResumesTheWaitingRun(t *testing.T) {
	b, st, runner, resumer, _, _ := newTestBus(t)
	ctx := testCtx()
	runner.setOutput("w1", "all clear")
	runID := id.NewAgentRunID()

	if _, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "status?"},
		AskerRunID: runID, ToolCallID: "call-1",
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	if err := b.deliverOne(ctx, queued[0].ID); err != nil {
		t.Fatalf("deliverOne: %v", err)
	}

	if resumer.count() != 1 {
		t.Fatalf("resumed %d times, want exactly 1", resumer.count())
	}
	got := resumer.last()
	if got.RunID != runID || got.CallID != "call-1" {
		t.Fatalf("resumed the wrong call: %+v", got)
	}

	var payload AskReply
	if err := json.Unmarshal([]byte(got.Result), &payload); err != nil {
		t.Fatalf("the resume result must be JSON the tool can return: %v", err)
	}
	if payload.Content != "all clear" || payload.Performative != string(Inform) || payload.Sender != "w1" {
		t.Fatalf("reply payload is wrong: %+v", payload)
	}
}

// A resumed asker already has the content, so an inbox copy of the same
// reply would be a duplicate.
func TestAResolvedReplyQueuesNoDelivery(t *testing.T) {
	b, st, runner, _, _, _ := newTestBus(t)
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

	if left, _ := st.ListQueuedDeliveries(ctx, 10); len(left) != 0 {
		t.Fatalf("%d deliveries queued for a reply that already resumed its asker", len(left))
	}
}

// A second reply carrying the same in-reply-to must be stored and must not
// resume anything. The claim is what makes that true.
func TestSecondReplyDoesNotResumeTwice(t *testing.T) {
	b, st, _, resumer, _, _ := newTestBus(t)
	ctx := testCtx()

	ask, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "status?"},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := b.Send(ctx, SendParams{
			Sender: Address{Agent: "w1"}, Receivers: []Address{{Agent: "planner"}},
			Performative: Inform, Content: "answer", ConversationID: ask.ConversationID, InReplyTo: ask.ReplyWith,
		}); err != nil {
			t.Fatalf("reply %d: %v", i, err)
		}
	}
	if resumer.count() != 1 {
		t.Fatalf("resumed %d times, want exactly 1", resumer.count())
	}
	msgs, _ := st.ListMessages(ctx, &MessageListFilter{Limit: 10})
	if len(msgs) != 3 {
		t.Fatalf("stored %d messages, want the ask plus both replies", len(msgs))
	}
}

// agree means "working on it". It must be delivered without resuming.
func TestAgreeDoesNotResume(t *testing.T) {
	b, _, _, resumer, _, _ := newTestBus(t)
	ctx := testCtx()

	ask, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "status?"},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "w1"}, Receivers: []Address{{Agent: "planner"}},
		Performative: Agree, Content: "on it", ConversationID: ask.ConversationID, InReplyTo: ask.ReplyWith,
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resumer.count() != 0 {
		t.Fatal("agree must not un-pause the asker")
	}
}

// A reply whose in-reply-to matches nothing is ordinary mail.
func TestUnmatchedReplyIsJustAMessage(t *testing.T) {
	b, _, _, resumer, _, _ := newTestBus(t)
	ctx := testCtx()

	res, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "w1"}, Receivers: []Address{{Agent: "planner"}},
		Performative: Inform, Content: "unsolicited", InReplyTo: "nobody-asked",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resumer.count() != 0 {
		t.Fatal("an unmatched in-reply-to must not resume anything")
	}
	if len(res.Deliveries) != 1 {
		t.Fatalf("got %d deliveries, want the message to be delivered normally", len(res.Deliveries))
	}
}
