package a2a

import (
	"testing"
)

func newTestBus(t *testing.T) (*Bus, *memStore, *fakeRunner, *fakeResumer, *recordingHooks, *fakeClock) {
	t.Helper()
	st, runner, res := newMemStore(), newFakeRunner(), newFakeResumer()
	hooks, clk := &recordingHooks{}, &fakeClock{now: testNow}
	b, err := NewBus(BusConfig{
		Store:       st,
		Runner:      runner,
		Resumer:     res,
		Hooks:       hooks,
		Clock:       clk,
		Synchronous: true,
		Options:     Options{HopCeiling: 3, Workers: 1},
	})
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}
	return b, st, runner, res, hooks, clk
}

func TestNewBusRequiresStoreAndRunner(t *testing.T) {
	if _, err := NewBus(BusConfig{Runner: newFakeRunner()}); !errorsIs(err, ErrNoStore) {
		t.Fatalf("err = %v, want ErrNoStore", err)
	}
	if _, err := NewBus(BusConfig{Store: newMemStore()}); !errorsIs(err, ErrNoRunner) {
		t.Fatalf("err = %v, want ErrNoRunner", err)
	}
}

func TestSendInformativeQueuesOneDeliveryPerReceiver(t *testing.T) {
	b, st, _, _, hooks, _ := newTestBus(t)
	ctx := testCtx()

	res, err := b.Send(ctx, SendParams{
		Sender:       Address{Agent: "planner"},
		Receivers:    []Address{{Agent: "w1"}, {Agent: "w2"}},
		Performative: Inform,
		Content:      "the build is green",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.MessageID.IsNil() || res.ConversationID.IsNil() {
		t.Fatalf("Send returned empty ids: %+v", res)
	}
	if len(res.Deliveries) != 2 {
		t.Fatalf("got %d delivery outcomes, want 2", len(res.Deliveries))
	}
	for _, d := range res.Deliveries {
		if d.Status != DeliveryQueued {
			t.Errorf("%s: status = %s, want queued", d.Receiver.Agent, d.Status)
		}
	}

	queued, err := st.ListQueuedDeliveries(ctx, 10)
	if err != nil {
		t.Fatalf("ListQueuedDeliveries: %v", err)
	}
	if len(queued) != 2 {
		t.Fatalf("store holds %d queued deliveries, want 2", len(queued))
	}
	if got := hooks.sent(); got != 1 {
		t.Fatalf("MessageSent fired %d times, want 1", got)
	}
}

func TestSendPersistsTheEnvelopeAndOpensAConversation(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	res, err := b.Send(ctx, SendParams{
		Sender:       Address{Agent: "planner"},
		Receivers:    []Address{{Agent: "w1"}},
		Performative: Inform,
		Content:      "hello",
		Protocol:     "fipa-request",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	msg, err := st.GetMessage(ctx, res.MessageID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg.Content != "hello" || msg.Performative != Inform {
		t.Fatalf("stored envelope is wrong: %+v", msg)
	}
	if msg.Hops != 1 {
		t.Fatalf("Hops = %d, want 1 for the first message in a conversation", msg.Hops)
	}
	if msg.Scope.IsZero() {
		t.Fatal("the envelope must carry the sender's scope")
	}

	conv, err := st.GetConversation(ctx, res.ConversationID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if !conv.IsOpen() || conv.Protocol != "fipa-request" || conv.HopCeiling != 3 {
		t.Fatalf("conversation is wrong: %+v", conv)
	}
	if !conv.HasParticipant(Address{Agent: "planner"}) || !conv.HasParticipant(Address{Agent: "w1"}) {
		t.Fatalf("both ends must be recorded as participants: %+v", conv.Participants)
	}
}

func TestSendJoinsAnExistingConversation(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	first, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "one",
	})
	if err != nil {
		t.Fatalf("first Send: %v", err)
	}
	second, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "two", ConversationID: first.ConversationID,
	})
	if err != nil {
		t.Fatalf("second Send: %v", err)
	}
	if second.ConversationID != first.ConversationID {
		t.Fatal("an explicit conversation id must be joined, not replaced")
	}

	msg, err := st.GetMessage(ctx, second.MessageID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if msg.Hops != 2 {
		t.Fatalf("Hops = %d, want 2 for the second message", msg.Hops)
	}
}

func TestSendRejectsAnInvalidEnvelopeBeforeWritingAnything(t *testing.T) {
	b, st, _, _, hooks, _ := newTestBus(t)
	ctx := testCtx()

	_, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "planner"}},
		Performative: Inform, Content: "talking to myself",
	})
	if !errorsIs(err, ErrSelfAddressed) {
		t.Fatalf("err = %v, want ErrSelfAddressed", err)
	}
	msgs, listErr := st.ListMessages(ctx, &MessageListFilter{Limit: 10})
	if listErr != nil {
		t.Fatalf("ListMessages: %v", listErr)
	}
	if len(msgs) != 0 {
		t.Fatalf("a refused send wrote %d messages, want 0", len(msgs))
	}
	if hooks.sent() != 0 {
		t.Fatal("a refused send must not fire MessageSent")
	}
}
