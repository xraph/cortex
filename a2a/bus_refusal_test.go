package a2a

import (
	"context"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// assertNoMessagesWritten is the guarantee every refusal shares: nothing
// reached the store, so nothing can be waiting on a message that is not there.
func assertNoMessagesWritten(t *testing.T, ctx context.Context, st *memStore) {
	t.Helper()
	msgs, err := st.ListMessages(ctx, &MessageListFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("a refused send wrote %d messages, want 0", len(msgs))
	}
}

func TestSendRefusesAClosedConversation(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	first, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "one",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	conv, err := st.GetConversation(ctx, first.ConversationID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	conv.Status = StatusClosed
	if err := st.UpdateConversation(ctx, conv); err != nil {
		t.Fatalf("UpdateConversation: %v", err)
	}

	_, err = b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "two", ConversationID: first.ConversationID,
	})
	if !errorsIs(err, ErrConversationClosed) {
		t.Fatalf("err = %v, want ErrConversationClosed", err)
	}
}

func TestSendRefusesPastTheHopCeiling(t *testing.T) {
	b, _, _, _, _, _ := newTestBus(t) // ceiling is 3
	ctx := testCtx()

	var convID id.ConversationID
	for i := 0; i < 3; i++ {
		res, err := b.Send(ctx, SendParams{
			Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
			Performative: Inform, Content: "msg", ConversationID: convID,
		})
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		convID = res.ConversationID
	}

	_, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "one too many", ConversationID: convID,
	})
	if !errorsIs(err, ErrHopCeiling) {
		t.Fatalf("err = %v, want ErrHopCeiling", err)
	}
}

func TestSendRefusesAnUnroutableAddress(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	// Only the in-process transport is configured, and it handles local
	// addresses. A Node names a peer nothing here can reach.
	_, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1", Node: "peer.example"}},
		Performative: Inform, Content: "hello over there",
	})
	if !errorsIs(err, ErrUnroutable) {
		t.Fatalf("err = %v, want ErrUnroutable", err)
	}
	assertNoMessagesWritten(t, ctx, st)
}

func TestSendRequiresAScope(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)

	_, err := b.Send(context.Background(), SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "no scope here",
	})
	if !errorsIs(err, cortex.ErrNoScope) {
		t.Fatalf("err = %v, want cortex.ErrNoScope", err)
	}
	assertNoMessagesWritten(t, testCtx(), st)
}

func TestSendRefusesAnUnknownPerformativeBeforeOpeningAConversation(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	_, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: "shout", Content: "?",
	})
	if !errorsIs(err, ErrInvalidPerformative) {
		t.Fatalf("err = %v, want ErrInvalidPerformative", err)
	}
	convs, err := st.ListConversations(ctx, &ConversationListFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if len(convs) != 0 {
		t.Fatalf("a refused send opened %d conversations, want 0", len(convs))
	}
}
