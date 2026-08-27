package a2a

import (
	"encoding/json"
	"testing"

	"github.com/xraph/cortex/id"
)

func TestCancelClosesTheConversationAndFailsWaitingAsks(t *testing.T) {
	b, st, _, resumer, _, _ := newTestBus(t)
	ctx := testCtx()

	ask, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "status?"},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if _, sendErr := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Cancel, Content: "never mind", ConversationID: ask.ConversationID,
	}); sendErr != nil {
		t.Fatalf("cancel Send: %v", sendErr)
	}

	// Deliver the cancel only. The original request is still queued, which
	// is exactly the state a real cancel races with.
	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	for _, d := range queued {
		msg, getErr := st.GetMessage(ctx, d.MessageID)
		if getErr != nil {
			t.Fatalf("GetMessage: %v", getErr)
		}
		if msg.Performative != Cancel {
			continue
		}
		if delErr := b.deliverOne(ctx, d.ID); delErr != nil {
			t.Fatalf("deliverOne: %v", delErr)
		}
	}

	conv, err := st.GetConversation(ctx, ask.ConversationID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if conv.Status != StatusClosed {
		t.Fatalf("Status = %s, want closed", conv.Status)
	}
	if resumer.count() != 1 {
		t.Fatalf("resumed %d times, want 1 (the cancelled ask)", resumer.count())
	}
	var payload AskReply
	if err := json.Unmarshal([]byte(resumer.last().Result), &payload); err != nil {
		t.Fatalf("unmarshal resume payload: %v", err)
	}
	if !lastReplyContains(payload, "cancelled") {
		t.Fatalf("a cancelled ask must resume saying so: %+v", payload.Replies)
	}
}

func TestCancelStartsNoRun(t *testing.T) {
	b, st, runner, _, _, _ := newTestBus(t)
	ctx := testCtx()

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Cancel, Content: "stop",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	if err := b.deliverOne(ctx, queued[0].ID); err != nil {
		t.Fatalf("deliverOne: %v", err)
	}
	if runner.callCount() != 0 {
		t.Fatal("handing an LLM a bookkeeping message is a wasted call")
	}
}
