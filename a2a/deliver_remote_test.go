package a2a

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/xraph/cortex/id"
)

// fakeTransport records what it was asked to carry.
type fakeTransport struct {
	mu      sync.Mutex
	node    string
	carried []*Envelope
	err     error
}

func (f *fakeTransport) Handles(addr Address) bool { return addr.Node == f.node }

func (f *fakeTransport) Deliver(_ context.Context, e *Envelope, _ Address) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	cp := *e
	f.carried = append(f.carried, &cp)
	return nil
}

func (f *fakeTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.carried)
}

func (f *fakeTransport) last() *Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.carried) == 0 {
		return nil
	}
	return f.carried[len(f.carried)-1]
}

func TestRemoteReceiverGoesToTheTransport(t *testing.T) {
	b, _, runner, _, _, _ := newTestBus(t)
	ctx := testCtx()
	tr := &fakeTransport{node: "peer.example"}
	b.AddTransport(tr)

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "worker", Node: "peer.example"}},
		Performative: Request, Content: "over there please",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := b.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	// The local runner must not have been used. A remote address answered
	// by a local agent of the same name is the bug this test exists for.
	if runner.callCount() != 0 {
		t.Fatalf("the local runner ran %d times for a remote address", runner.callCount())
	}
	if tr.count() != 1 || tr.last().Content != "over there please" {
		t.Fatalf("transport carried %d messages: %+v", tr.count(), tr.last())
	}
}

func TestRemoteDeliveryFailureDoesNotRunLocally(t *testing.T) {
	b, st, runner, _, hooks, _ := newTestBus(t)
	ctx := testCtx()
	b.AddTransport(&fakeTransport{node: "peer.example", err: errors.New("peer unreachable")})

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "worker", Node: "peer.example"}},
		Performative: Request, Content: "hello?",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := b.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if runner.callCount() != 0 {
		t.Fatal("a failed remote delivery must never fall back to a local agent")
	}
	if hooks.refused() != 1 {
		t.Fatalf("MessageRefused fired %d times, want 1", hooks.refused())
	}
	rows, err := st.ListQueuedDeliveries(ctx, 10)
	if err != nil {
		t.Fatalf("ListQueuedDeliveries: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("%d rows left queued after a failed delivery", len(rows))
	}
}

func TestRemoteAddressWithNoTransportIsRefusedAtSend(t *testing.T) {
	b, _, _, _, _, _ := newTestBus(t)
	ctx := testCtx()

	// No transport registered for that node, so the send never happens:
	// an agent cannot invent a hostname and have cortex reach for it.
	_, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "worker", Node: "nowhere.example"}},
		Performative: Request, Content: "hello?",
	})
	if !errorsIs(err, ErrUnroutable) {
		t.Fatalf("err = %v, want ErrUnroutable", err)
	}
}

// A remote ask is carried by the transport, and answered later by an
// ordinary inbound message. That is the same path a local reply takes,
// which is what makes a resumed agent unable to tell the difference.
func TestRemoteAskIsResolvedByAReplyThroughTheBus(t *testing.T) {
	b, _, _, resumer, _, _ := newTestBus(t)
	ctx := testCtx()
	b.AddTransport(&fakeTransport{node: "peer.example"})

	ask, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "worker", Node: "peer.example"}},
			Content: "status?",
		},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, drainErr := b.Drain(ctx); drainErr != nil {
		t.Fatalf("Drain: %v", drainErr)
	}
	if resumer.count() != 0 {
		t.Fatal("carrying the question must not resume the asker")
	}

	// The peer answers, and the client feeds it back in here.
	if _, sendErr := b.Send(ctx, SendParams{
		Sender: Address{Agent: "worker", Node: "peer.example"}, Receivers: []Address{{Agent: "planner"}},
		Performative: Inform, Content: "all clear over here",
		ConversationID: ask.ConversationID, InReplyTo: ask.ReplyWith,
	}); sendErr != nil {
		t.Fatalf("reply Send: %v", sendErr)
	}
	if resumer.count() != 1 {
		t.Fatalf("resumed %d times, want 1", resumer.count())
	}
}

// A failed remote delivery has to reach a waiting asker, or the run sits
// until its deadline learning nothing.
func TestRemoteDeliveryFailureResolvesTheWaitingAsk(t *testing.T) {
	b, _, _, resumer, _, _ := newTestBus(t)
	ctx := testCtx()
	b.AddTransport(&fakeTransport{node: "peer.example", err: errors.New("connection refused")})

	if _, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{
			Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "worker", Node: "peer.example"}},
			Content: "status?",
		},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := b.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if resumer.count() != 1 {
		t.Fatalf("resumed %d times, want 1: an unreachable peer is something the asker can act on", resumer.count())
	}
}
