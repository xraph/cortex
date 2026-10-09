package a2a

import (
	"context"
	"testing"

	"github.com/xraph/cortex/id"
)

// askCheckingStore fails a delivery written before the pending ask its
// question is waiting on. That delivery is the race this order closes: a
// quick peer answers it, and the answer finds nothing to claim.
type askCheckingStore struct {
	*memStore
	t *testing.T
}

func (s askCheckingStore) CreateDelivery(ctx context.Context, d *Delivery) error {
	msg, err := s.GetMessage(ctx, d.MessageID)
	if err != nil {
		s.t.Fatalf("a delivery points at a message that is not there: %v", err)
	}
	if msg.ReplyWith != "" {
		s.mu.Lock()
		_, waiting := s.asks[msg.ReplyWith]
		s.mu.Unlock()
		if !waiting {
			s.t.Fatal("the question was queued before its pending ask was written; an answer could arrive with nothing to resume")
		}
	}
	return s.memStore.CreateDelivery(ctx, d)
}

func TestAskWritesThePendingAskBeforeQueuingTheQuestion(t *testing.T) {
	st := askCheckingStore{memStore: newMemStore(), t: t}
	b, err := NewBus(BusConfig{
		Store: st, Runner: newFakeRunner(), Resumer: newFakeResumer(),
		Synchronous: true, Options: Options{HopCeiling: 3, Workers: 1},
	})
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}

	if _, err := b.Ask(testCtx(), AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "status?"},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
}

// A held ask is on the record and waiting, and nobody has been asked.
// Release is what asks them, once.
func TestAHeldAskQueuesNothingUntilReleased(t *testing.T) {
	b, st, _, _, hooks, _ := newTestBus(t)
	ctx := testCtx()

	res, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "status?"},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
		Hold: true,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if _, err := st.GetMessage(ctx, res.MessageID); err != nil {
		t.Fatalf("a held ask must still be on the record: %v", err)
	}
	if queued, _ := st.ListQueuedDeliveries(ctx, 10); len(queued) != 0 {
		t.Fatalf("a held ask queued %d deliveries; the asker has not paused yet", len(queued))
	}
	if hooks.sent() != 0 {
		t.Fatal("MessageSent fired for a question nobody has been sent")
	}

	if err := b.Release(ctx, res); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if queued, _ := st.ListQueuedDeliveries(ctx, 10); len(queued) != 1 {
		t.Fatalf("Release queued %d deliveries, want 1", len(queued))
	}

	// A second release would carry the question twice.
	if err := b.Release(ctx, res); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	if queued, _ := st.ListQueuedDeliveries(ctx, 10); len(queued) != 1 {
		t.Fatalf("a second Release left %d deliveries queued, want still 1", len(queued))
	}
	if hooks.sent() != 1 {
		t.Fatalf("MessageSent fired %d times, want once", hooks.sent())
	}
}

// A proposal is an answer and a new ask at once. Held, it must not resume
// the initiator either: the initiator's award answers the proposer, and
// an award that arrives before the proposer pauses is the same lost
// answer one step further down the conversation.
func TestAHeldAnswerResumesNobodyUntilReleased(t *testing.T) {
	b, _, _, resumer, _, _ := newTestBus(t)
	ctx := testCtx()

	cfp, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender: Address{Agent: "initiator"}, Receivers: []Address{{Agent: "w1"}},
			Performative: CFP, Content: "who can do it?",
		},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-cfp",
	})
	if err != nil {
		t.Fatalf("Ask (cfp): %v", err)
	}

	proposal, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender: Address{Agent: "w1"}, Receivers: []Address{{Agent: "initiator"}},
			Performative: Propose, Content: "I can, by Friday",
			ConversationID: cfp.ConversationID, InReplyTo: cfp.ReplyWith,
		},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-propose",
		Hold: true,
	})
	if err != nil {
		t.Fatalf("Ask (propose): %v", err)
	}
	if resumer.count() != 0 {
		t.Fatal("a held proposal resumed the initiator before the proposer had paused")
	}

	if err := b.Release(ctx, proposal); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if resumer.count() != 1 {
		t.Fatalf("releasing the proposal resumed %d runs, want the initiator", resumer.count())
	}
}

// Release on an ask that was never held has nothing to send.
func TestReleaseIgnoresAnAskThatWasNotHeld(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	res, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "status?"},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if err := b.Release(ctx, res); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := b.Release(ctx, nil); err != nil {
		t.Fatalf("Release(nil): %v", err)
	}
	if queued, _ := st.ListQueuedDeliveries(ctx, 10); len(queued) != 1 {
		t.Fatalf("got %d queued deliveries, want the 1 the ask queued when it was sent", len(queued))
	}
}
