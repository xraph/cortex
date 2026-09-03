package a2a

import "testing"

func TestInboxReturnsEnvelopesAndMarksThemRead(t *testing.T) {
	b, _, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "note one",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := b.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	items, err := b.Inbox(ctx, "w1", InboxFilter{UnreadOnly: true})
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].Content != "note one" || items[0].Sender != "planner" {
		t.Fatalf("item is wrong: %+v", items[0])
	}
	if items[0].Performative != string(Inform) || items[0].ReceivedAt == "" {
		t.Fatalf("item lost its envelope detail: %+v", items[0])
	}

	// Reading is the acknowledgement, so a second call comes back empty.
	again, err := b.Inbox(ctx, "w1", InboxFilter{UnreadOnly: true})
	if err != nil {
		t.Fatalf("second Inbox: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("got %d items on the second read, want 0", len(again))
	}
}

func TestInboxLeavesUndeliveredRowsAlone(t *testing.T) {
	b, _, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "still queued",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	items, err := b.Inbox(ctx, "w1", InboxFilter{UnreadOnly: true})
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(items) != 0 {
		t.Fatal("a queued delivery has not arrived yet and must not show in an inbox")
	}
}
