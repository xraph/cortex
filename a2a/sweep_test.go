package a2a

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
)

func TestSweepResolvesAnOverdueAskIntoAFailure(t *testing.T) {
	b, _, _, resumer, _, clk := newTestBus(t)
	ctx := testCtx()

	deadline := testNow.Add(time.Minute)
	if _, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
			Content: "status?", ReplyBy: &deadline,
		},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	n, err := b.SweepExpiredAsks(ctx)
	if err != nil {
		t.Fatalf("SweepExpiredAsks: %v", err)
	}
	if n != 0 {
		t.Fatalf("swept %d asks before the deadline, want 0", n)
	}

	clk.advance(2 * time.Minute)
	n, err = b.SweepExpiredAsks(ctx)
	if err != nil {
		t.Fatalf("SweepExpiredAsks: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d asks after the deadline, want 1", n)
	}
	if resumer.count() != 1 {
		t.Fatalf("resumed %d times, want 1", resumer.count())
	}

	var payload AskReply
	if err := json.Unmarshal([]byte(resumer.last().Result), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !lastReplyContains(payload, "deadline") {
		t.Fatalf("a swept ask must resume with a timeout failure, got %+v", payload.Replies)
	}
}

func TestSweepIsIdempotent(t *testing.T) {
	b, _, _, resumer, _, clk := newTestBus(t)
	ctx := testCtx()

	if _, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "?"},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	clk.advance(DefaultReplyBy + time.Minute)

	for i := 0; i < 3; i++ {
		if _, err := b.SweepExpiredAsks(ctx); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
	if resumer.count() != 1 {
		t.Fatalf("resumed %d times across three sweeps, want 1", resumer.count())
	}
}

// A reply that lands after the sweep gave up must not resume the run a
// second time. The claim already went to the sweep.
func TestReplyAfterSweepDoesNotResumeAgain(t *testing.T) {
	b, _, _, resumer, _, clk := newTestBus(t)
	ctx := testCtx()

	ask, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "?"},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	clk.advance(DefaultReplyBy + time.Minute)
	if _, err := b.SweepExpiredAsks(ctx); err != nil {
		t.Fatalf("SweepExpiredAsks: %v", err)
	}

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "w1"}, Receivers: []Address{{Agent: "planner"}},
		Performative: Inform, Content: "sorry, took a while", ConversationID: ask.ConversationID, InReplyTo: ask.ReplyWith,
	}); err != nil {
		t.Fatalf("late reply: %v", err)
	}
	if resumer.count() != 1 {
		t.Fatalf("resumed %d times, want 1", resumer.count())
	}
}
