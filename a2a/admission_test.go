package a2a

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// fakeAdmitter stands in for an engine's run admission. While stopping it
// refuses a fresh context and admits one it already admitted, which is how
// the engine treats a resume that follows a claim made under a ticket.
type fakeAdmitter struct {
	mu       sync.Mutex
	stopping bool
	admitted int
	refused  int
}

type fakeTicketKey struct{}

func (a *fakeAdmitter) AdmitRun(ctx context.Context) (context.Context, func(), error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ctx.Value(fakeTicketKey{}) == nil && a.stopping {
		a.refused++
		return ctx, func() {}, cortex.ErrEngineStopping
	}
	a.admitted++
	return context.WithValue(ctx, fakeTicketKey{}, true), func() {}, nil
}

func (a *fakeAdmitter) setStopping(v bool) {
	a.mu.Lock()
	a.stopping = v
	a.mu.Unlock()
}

// newAdmittedBus builds a second bus over st, standing in for another
// worker that shares the database.
func newAdmittedBus(t *testing.T, st *memStore, runner *fakeRunner, resumer *fakeResumer, admitter Admitter) *Bus {
	t.Helper()
	b, err := NewBus(BusConfig{
		Store:       st,
		Runner:      runner,
		Resumer:     resumer,
		Admitter:    admitter,
		Clock:       &fakeClock{now: testNow},
		Synchronous: true,
		Options:     Options{HopCeiling: 3, Workers: 1},
	})
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}
	return b
}

func onlyDelivery(t *testing.T, st *memStore) *Delivery {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.deliveries) != 1 {
		t.Fatalf("store holds %d deliveries, want 1", len(st.deliveries))
	}
	for _, d := range st.deliveries {
		cp := *d
		return &cp
	}
	return nil
}

func askClaimed(st *memStore, replyWith string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	a, ok := st.asks[replyWith]
	return ok && a.ClaimedAt != nil
}

// A directive that arrives while the host drains is not claimed here. The
// row stays queued, and the next worker runs it exactly once.
func TestDrainLeavesDirectiveQueuedWhileStopping(t *testing.T) {
	st := newMemStore()
	ctx := testCtx()
	stopping := &fakeAdmitter{stopping: true}
	here := newAdmittedBus(t, st, newFakeRunner(), newFakeResumer(), stopping)

	if _, err := here.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Request, Content: "report status", ReplyWith: "rw-1",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if _, err := here.Drain(ctx); !errors.Is(err, cortex.ErrEngineStopping) {
		t.Fatalf("Drain while stopping = %v, want ErrEngineStopping", err)
	}
	if d := onlyDelivery(t, st); d.State != DeliveryQueued || d.ClaimedAt != nil {
		t.Fatalf("delivery = %s (claimed %v), want queued and unclaimed", d.State, d.ClaimedAt)
	}

	otherRunner := newFakeRunner()
	other := newAdmittedBus(t, st, otherRunner, newFakeResumer(), &fakeAdmitter{})
	if _, err := other.Drain(ctx); err != nil {
		t.Fatalf("other worker Drain: %v", err)
	}
	if got := otherRunner.callCount(); got != 1 {
		t.Fatalf("other worker ran the directive %d times, want 1", got)
	}
	if replies := repliesFrom(st, "w1"); len(replies) != 1 || replies[0].Performative != Inform {
		t.Fatalf("replies from w1 = %+v, want one inform", replies)
	}
}

// A directive this worker already claimed, whose run the host then refuses,
// goes back to the queue with no reply. The asker hears nothing from this
// worker, and the next worker runs the directive exactly once.
func TestRefusedDirectiveIsRequeuedWithoutReply(t *testing.T) {
	st := newMemStore()
	ctx := testCtx()
	refusing := newFakeRunner()
	refusing.err = cortex.ErrEngineStopping
	here := newAdmittedBus(t, st, refusing, newFakeResumer(), nil)

	if _, err := here.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Request, Content: "report status", ReplyWith: "rw-1",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := here.Drain(ctx); !errors.Is(err, cortex.ErrEngineStopping) {
		t.Fatalf("Drain = %v, want ErrEngineStopping", err)
	}
	if d := onlyDelivery(t, st); d.State != DeliveryQueued || d.ClaimedAt != nil || d.Error != "" {
		t.Fatalf("delivery = %+v, want it back in the queue untouched", d)
	}
	if replies := repliesFrom(st, "w1"); len(replies) != 0 {
		t.Fatalf("a refused run produced replies %+v, want none", replies)
	}

	otherRunner := newFakeRunner()
	other := newAdmittedBus(t, st, otherRunner, newFakeResumer(), nil)
	if _, err := other.Drain(ctx); err != nil {
		t.Fatalf("other worker Drain: %v", err)
	}
	if got := otherRunner.callCount(); got != 1 {
		t.Fatalf("other worker ran the directive %d times, want 1", got)
	}
	if replies := repliesFrom(st, "w1"); len(replies) != 1 || replies[0].Performative != Inform {
		t.Fatalf("replies from w1 = %+v, want exactly one inform", replies)
	}
}

// A directive whose run is handed off before it answers is no failure:
// the run is reclaimed and continued elsewhere. No Failure reply goes to
// the asker, and the delivery is not run a second time.
func TestHandedOffDirectiveSendsNoFailure(t *testing.T) {
	st := newMemStore()
	ctx := testCtx()
	interrupted := newFakeRunner()
	interrupted.err = fmt.Errorf("worker draining: %w", cortex.ErrRunInterrupted)
	here := newAdmittedBus(t, st, interrupted, newFakeResumer(), nil)

	if _, err := here.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w2"}},
		Performative: Request, Content: "report status", ReplyWith: "rw-1",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := here.Drain(ctx); err != nil {
		t.Fatalf("Drain = %v, want nil", err)
	}
	if replies := repliesFrom(st, "w2"); len(replies) != 0 {
		t.Fatalf("a handed-off run produced replies %+v, want none", replies)
	}
	if d := onlyDelivery(t, st); d.State != DeliveryDelivered || d.RunID.IsNil() {
		t.Fatalf("delivery = %+v, want delivered to the handed-off run", d)
	}
	if _, err := here.Drain(ctx); err != nil {
		t.Fatalf("second Drain: %v", err)
	}
	if got := interrupted.callCount(); got != 1 {
		t.Fatalf("the directive ran %d times, want 1", got)
	}
}

// A directive admitted before the host started draining finishes here: its
// reply resumes the asker even though the host is now stopping, because
// the resume runs under the admission taken before the claim.
func TestAdmittedDirectiveResumesAskerAfterDrainStarts(t *testing.T) {
	st := newMemStore()
	ctx := testCtx()
	admitter := &fakeAdmitter{}
	runner, resumer := newFakeRunner(), newFakeResumer()
	runner.respond = func(_, _ string) string {
		admitter.setStopping(true)
		return "all clear"
	}
	b := newAdmittedBus(t, st, runner, resumer, admitter)
	runID := id.NewAgentRunID()

	if _, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "status?"},
		AskerRunID: runID, ToolCallID: "call-1",
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := b.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if resumer.count() != 1 || resumer.last().RunID != runID {
		t.Fatalf("resumes = %d, want the asker resumed once", resumer.count())
	}
}

// A reply sent from outside any run while the host drains does not claim
// the ask. It is queued instead, and the worker that carries it resumes
// the asker once.
func TestReplyWhileStoppingLeavesAskForAnotherWorker(t *testing.T) {
	st := newMemStore()
	ctx := testCtx()
	admitter := &fakeAdmitter{}
	resumer := newFakeResumer()
	b := newAdmittedBus(t, st, newFakeRunner(), resumer, admitter)

	ask, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "status?"},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	// The question's own delivery is not what this test is about.
	st.mu.Lock()
	for _, d := range st.deliveries {
		d.State = DeliveryDelivered
	}
	st.mu.Unlock()

	admitter.setStopping(true)
	if _, err = b.Send(ctx, SendParams{
		Sender: Address{Agent: "w1"}, Receivers: []Address{{Agent: "planner"}},
		Performative: Inform, Content: "all clear", ConversationID: ask.ConversationID, InReplyTo: ask.ReplyWith,
	}); err != nil {
		t.Fatalf("Send reply while stopping: %v", err)
	}
	if askClaimed(st, ask.ReplyWith) {
		t.Fatal("a reply refused for stopping still claimed the ask")
	}
	if resumer.count() != 0 {
		t.Fatalf("resumed %d times while stopping, want 0", resumer.count())
	}
	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	if len(queued) != 1 {
		t.Fatalf("%d deliveries queued for the refused reply, want 1", len(queued))
	}

	otherResumer := newFakeResumer()
	other := newAdmittedBus(t, st, newFakeRunner(), otherResumer, &fakeAdmitter{})
	if _, err = other.Drain(ctx); err != nil {
		t.Fatalf("other worker Drain: %v", err)
	}
	if otherResumer.count() != 1 {
		t.Fatalf("other worker resumed %d times, want 1", otherResumer.count())
	}
	if !askClaimed(st, ask.ReplyWith) {
		t.Fatal("the ask is still unclaimed after the asker was resumed")
	}
}

// An overdue ask is not claimed by a sweep while the host drains. The pass
// ends without an error, and the next sweep resolves it once.
func TestSweepLeavesExpiredAskWhileStopping(t *testing.T) {
	st := newMemStore()
	ctx := testCtx()
	admitter := &fakeAdmitter{stopping: true}
	clk := &fakeClock{now: testNow}
	resumer := newFakeResumer()
	b, err := NewBus(BusConfig{Store: st, Runner: newFakeRunner(), Resumer: resumer, Admitter: admitter, Clock: clk, Synchronous: true, Options: Options{HopCeiling: 3, Workers: 1}})
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}
	deadline := testNow.Add(time.Minute)
	ask, err := b.Ask(ctx, AskParams{
		SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}}, Content: "status?", ReplyBy: &deadline},
		AskerRunID: id.NewAgentRunID(), ToolCallID: "call-1",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	clk.advance(2 * time.Minute)

	n, err := b.SweepExpiredAsks(ctx)
	if err != nil || n != 0 {
		t.Fatalf("SweepExpiredAsks while stopping = %d, %v; want 0, nil", n, err)
	}
	if askClaimed(st, ask.ReplyWith) || resumer.count() != 0 {
		t.Fatal("a sweep refused for stopping still claimed or resumed the ask")
	}

	admitter.setStopping(false)
	if n, err = b.SweepExpiredAsks(ctx); err != nil || n != 1 {
		t.Fatalf("SweepExpiredAsks after = %d, %v; want 1, nil", n, err)
	}
	if resumer.count() != 1 {
		t.Fatalf("resumed %d times, want 1", resumer.count())
	}
}

// repliesFrom lists the messages a sender wrote, in no particular order.
func repliesFrom(st *memStore, sender string) []*Envelope {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []*Envelope
	for _, e := range st.messages {
		if e.Sender.Agent == sender {
			out = append(out, e)
		}
	}
	return out
}
