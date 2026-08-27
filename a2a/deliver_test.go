package a2a

import (
	"errors"
	"strings"
	"testing"
)

func TestDeliverInformativeLandsInTheInboxAndStartsNoRun(t *testing.T) {
	b, st, runner, _, hooks, _ := newTestBus(t)
	ctx := testCtx()

	res, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "the build is green",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	if err := b.deliverOne(ctx, queued[0].ID); err != nil {
		t.Fatalf("deliverOne: %v", err)
	}

	if runner.callCount() != 0 {
		t.Fatal("an informative must not start a run")
	}
	inbox, err := st.ListInbox(ctx, "w1", InboxFilter{UnreadOnly: true})
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(inbox) != 1 || inbox[0].MessageID != res.MessageID {
		t.Fatalf("inbox = %+v, want the sent message", inbox)
	}
	if hooks.delivered() != 1 {
		t.Fatalf("MessageDelivered fired %d times, want 1", hooks.delivered())
	}
}

func TestDeliverDirectiveStartsARunAndRepliesWithItsOutput(t *testing.T) {
	b, st, runner, _, _, _ := newTestBus(t)
	ctx := testCtx()
	runner.setOutput("w1", "status: green")

	res, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Request, Content: "report status", ReplyWith: "rw-1",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	if err := b.deliverOne(ctx, queued[0].ID); err != nil {
		t.Fatalf("deliverOne: %v", err)
	}

	if runner.callCount() != 1 {
		t.Fatalf("runner called %d times, want 1", runner.callCount())
	}
	if !strings.Contains(runner.lastInput(), "report status") {
		t.Fatalf("the rendered input lost the content: %q", runner.lastInput())
	}

	msgs, err := st.ListMessages(ctx, &MessageListFilter{ConversationID: res.ConversationID, Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("conversation holds %d messages, want the request and its reply", len(msgs))
	}
	reply := msgs[1]
	if reply.Performative != Inform || reply.InReplyTo != "rw-1" {
		t.Fatalf("reply is wrong: %+v", reply)
	}
	if reply.Content != "status: green" || reply.Sender.Agent != "w1" {
		t.Fatalf("reply lost the run output: %+v", reply)
	}
}

func TestDeliverDirectiveWhoseRunFailsRepliesWithFailure(t *testing.T) {
	b, st, runner, _, _, _ := newTestBus(t)
	ctx := testCtx()
	runner.setErr(errors.New("model exploded"))

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Request, Content: "report status", ReplyWith: "rw-1",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	if err := b.deliverOne(ctx, queued[0].ID); err != nil {
		t.Fatalf("deliverOne must not surface the peer's failure as its own error: %v", err)
	}

	msgs, _ := st.ListMessages(ctx, &MessageListFilter{Limit: 10})
	reply := msgs[len(msgs)-1]
	if reply.Performative != Failure {
		t.Fatalf("Performative = %s, want failure", reply.Performative)
	}
	if !strings.Contains(reply.Content, "model exploded") {
		t.Fatalf("the failure must carry the error text, got %q", reply.Content)
	}
}

func TestDeliverMarksTheRowDelivered(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "x",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	if err := b.deliverOne(ctx, queued[0].ID); err != nil {
		t.Fatalf("deliverOne: %v", err)
	}
	if left, _ := st.ListQueuedDeliveries(ctx, 10); len(left) != 0 {
		t.Fatalf("%d rows still queued after delivery, want 0", len(left))
	}
}

func TestDeliverAnAlreadyClaimedRowIsRefused(t *testing.T) {
	b, st, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "x",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	if err := b.deliverOne(ctx, queued[0].ID); err != nil {
		t.Fatalf("first deliverOne: %v", err)
	}
	if err := b.deliverOne(ctx, queued[0].ID); !errorsIs(err, ErrDeliveryAlreadyClaimed) {
		t.Fatalf("err = %v, want ErrDeliveryAlreadyClaimed", err)
	}
}

func TestRenderInputCarriesSenderPerformativeAndContent(t *testing.T) {
	e := &Envelope{
		Performative: Request,
		Sender:       Address{Agent: "planner"},
		Content:      "summarise the incident",
		Ontology:     "ops",
	}
	got := RenderInput(e)
	for _, want := range []string{"planner", "request", "summarise the incident", "ops"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered input is missing %q:\n%s", want, got)
		}
	}
}
