package a2a

import (
	"context"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

func testCtx() context.Context {
	return cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "tenant", Value: "acme"}},
	})
}

func TestMemStoreRoundTripsAConversation(t *testing.T) {
	ctx, s := testCtx(), newMemStore()
	c := &Conversation{
		ID:         id.NewConversationID(),
		Status:     StatusOpen,
		HopCeiling: 8,
		Initiator:  Address{Agent: "planner"},
	}
	if err := s.CreateConversation(ctx, c); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	got, err := s.GetConversation(ctx, c.ID)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if got.Status != StatusOpen || got.HopCeiling != 8 {
		t.Fatalf("round trip lost fields: %+v", got)
	}
}

func TestConversationParticipants(t *testing.T) {
	c := &Conversation{Status: StatusOpen}
	c.AddParticipant(Address{Agent: "a"})
	c.AddParticipant(Address{Agent: "a"})
	c.AddParticipant(Address{Agent: "b"})
	if len(c.Participants) != 2 {
		t.Fatalf("participants = %+v, want a and b once each", c.Participants)
	}
	if !c.HasParticipant(Address{Agent: "b"}) {
		t.Fatal("b should be a participant")
	}
	if c.HasParticipant(Address{Agent: "c"}) {
		t.Fatal("c never took part")
	}
}

// The claim is the whole point of the pending-ask table. Two callers race
// for one row and exactly one of them may resume the run.
func TestClaimPendingAskSucceedsOnce(t *testing.T) {
	ctx, s := testCtx(), newMemStore()
	ask := &PendingAsk{
		ReplyWith:  "rw-1",
		AskerRunID: id.NewAgentRunID(),
		ToolCallID: "call-1",
		Expected:   Address{Agent: "worker"},
	}
	if err := s.CreatePendingAsk(ctx, ask); err != nil {
		t.Fatalf("CreatePendingAsk: %v", err)
	}

	first, err := s.ClaimPendingAsk(ctx, "rw-1")
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if first.ToolCallID != "call-1" {
		t.Fatalf("claim returned the wrong row: %+v", first)
	}

	if _, err := s.ClaimPendingAsk(ctx, "rw-1"); !errorsIs(err, ErrAskAlreadyClaimed) {
		t.Fatalf("second claim: err = %v, want ErrAskAlreadyClaimed", err)
	}
}

func TestClaimUnknownAsk(t *testing.T) {
	ctx, s := testCtx(), newMemStore()
	if _, err := s.ClaimPendingAsk(ctx, "nope"); !errorsIs(err, ErrAskNotFound) {
		t.Fatalf("err = %v, want ErrAskNotFound", err)
	}
}

func TestListInboxReturnsUnreadOnly(t *testing.T) {
	ctx, s := testCtx(), newMemStore()
	d := &Delivery{
		ID:        id.NewDeliveryID(),
		MessageID: id.NewMessageID(),
		Receiver:  Address{Agent: "worker"},
		State:     DeliveryDelivered,
	}
	if err := s.CreateDelivery(ctx, d); err != nil {
		t.Fatalf("CreateDelivery: %v", err)
	}

	got, err := s.ListInbox(ctx, "worker", InboxFilter{UnreadOnly: true})
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d deliveries, want 1", len(got))
	}

	if readErr := s.MarkDeliveryRead(ctx, got[0].ID); readErr != nil {
		t.Fatalf("MarkDeliveryRead: %v", readErr)
	}
	got, err = s.ListInbox(ctx, "worker", InboxFilter{UnreadOnly: true})
	if err != nil {
		t.Fatalf("ListInbox after read: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d unread after marking read, want 0", len(got))
	}
}

func TestListQueuedDeliveriesIsWhatRedriveReadsFrom(t *testing.T) {
	ctx, s := testCtx(), newMemStore()
	queued := &Delivery{ID: id.NewDeliveryID(), MessageID: id.NewMessageID(), Receiver: Address{Agent: "w1"}, State: DeliveryQueued}
	done := &Delivery{ID: id.NewDeliveryID(), MessageID: id.NewMessageID(), Receiver: Address{Agent: "w2"}, State: DeliveryDelivered}
	for _, d := range []*Delivery{queued, done} {
		if err := s.CreateDelivery(ctx, d); err != nil {
			t.Fatalf("CreateDelivery: %v", err)
		}
	}
	got, err := s.ListQueuedDeliveries(ctx, 10)
	if err != nil {
		t.Fatalf("ListQueuedDeliveries: %v", err)
	}
	if len(got) != 1 || got[0].Receiver.Agent != "w1" {
		t.Fatalf("redrive must see only queued rows, got %+v", got)
	}
}
