package a2a

import (
	"testing"
	"time"

	"github.com/xraph/cortex/id"
)

func TestAskWritesAPendingAskKeyedByReplyWith(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()
	runID := id.NewAgentRunID()

	res, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
			Content: "what is the status?",
		},
		AskerRunID: runID,
		ToolCallID: "call-7",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if res.ReplyWith == "" {
		t.Fatal("Ask must mint a reply-with token")
	}

	ask, err := st.ClaimPendingAsk(ctx, res.ReplyWith)
	if err != nil {
		t.Fatalf("ClaimPendingAsk: %v", err)
	}
	if ask.AskerRunID != runID || ask.ToolCallID != "call-7" {
		t.Fatalf("pending ask lost its correlation: %+v", ask)
	}
	if ask.Expected.Agent != "w1" {
		t.Fatalf("Expected = %s, want w1", ask.Expected)
	}
	if ask.Deadline == nil {
		t.Fatal("an ask with no explicit ReplyBy must still get the default deadline")
	}
	if want := testNow.Add(DefaultReplyBy); !ask.Deadline.Equal(want) {
		t.Fatalf("Deadline = %s, want %s", ask.Deadline, want)
	}
}

func TestAskDefaultsToRequest(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	res, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "?"},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "c1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	msg, err := st.GetMessage(ctx, res.MessageID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg.Performative != Request {
		t.Fatalf("Performative = %s, want request", msg.Performative)
	}
	if msg.ReplyWith != res.ReplyWith {
		t.Fatal("the envelope must carry the same reply-with as the ledger row")
	}
}

// Several receivers is a call for proposals, which is a tender rather
// than a mistake.
func TestAskAcceptsMoreThanOneReceiver(t *testing.T) {
	b, _, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	res, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender:       Address{Agent: "planner"},
			Receivers:    []Address{{Agent: "w1"}, {Agent: "w2"}},
			Performative: CFP,
			Content:      "who can take this?",
		},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "c1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if res.ReplyWith == "" {
		t.Fatal("a tender needs a token to collect answers against")
	}
}

func TestAskRefusesAnInformativePerformative(t *testing.T) {
	b, _, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	_, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
			Performative: Inform, Content: "this answers nothing",
		},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "c1",
	})
	if !errorsIs(err, ErrAskNeedsDirective) {
		t.Fatalf("err = %v, want ErrAskNeedsDirective", err)
	}
}

func TestAskWritesNoLedgerRowWhenTheSendIsRefused(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	_, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "planner"}},
			Content: "?",
		},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "c1",
	})
	if !errorsIs(err, ErrSelfAddressed) {
		t.Fatalf("err = %v, want ErrSelfAddressed", err)
	}
	asks, err := st.ListExpiredAsks(ctx, testNow.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("ListExpiredAsks: %v", err)
	}
	if len(asks) != 0 {
		t.Fatalf("a refused ask left %d ledger rows, want 0", len(asks))
	}
}

func TestAskKeepsAnExplicitDeadline(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()
	deadline := testNow.Add(90 * time.Second)

	res, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
			Content: "?", ReplyBy: &deadline,
		},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "c1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	ask, err := st.ClaimPendingAsk(ctx, res.ReplyWith)
	if err != nil {
		t.Fatalf("ClaimPendingAsk: %v", err)
	}
	if !ask.Deadline.Equal(deadline) {
		t.Fatalf("Deadline = %s, want the explicit %s", ask.Deadline, deadline)
	}
}
