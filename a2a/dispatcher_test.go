package a2a

import (
	"sync"
	"testing"
	"time"
)

func TestDrainDeliversEverythingQueued(t *testing.T) {
	b, st, runner, _, _, _ := newTestBus(t)
	ctx := testCtx()

	for i := 0; i < 3; i++ {
		if _, err := b.Send(ctx, SendParams{
			Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
			Performative: Request, Content: "go",
		}); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	n, err := b.Drain(ctx)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// Three requests, and the three replies they produced. A drain that
	// stopped at the requests would leave the conversation half carried.
	if n != 6 {
		t.Fatalf("drained %d, want 6", n)
	}
	if runner.callCount() != 3 {
		t.Fatalf("runner called %d times, want 3", runner.callCount())
	}
	if left, _ := st.ListQueuedDeliveries(ctx, 10); len(left) != 0 {
		t.Fatalf("%d rows still queued after a drain", len(left))
	}
	inbox, _ := st.ListInbox(ctx, "planner", InboxFilter{UnreadOnly: true})
	if len(inbox) != 3 {
		t.Fatalf("planner has %d replies in the inbox, want 3", len(inbox))
	}
}

// Redrive is the restart story: rows queued by a process that died get
// picked up by the next one.
func TestRedrivePicksUpOrphanedDeliveries(t *testing.T) {
	st, runner := newMemStore(), newFakeRunner()
	ctx := testCtx()

	first, err := NewBus(BusConfig{Store: st, Runner: runner, Clock: &fakeClock{now: testNow}, Synchronous: true})
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}
	if _, sendErr := first.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Request, Content: "survive this",
	}); sendErr != nil {
		t.Fatalf("Send: %v", sendErr)
	}
	// first "crashes" here: nothing drained it.

	second, err := NewBus(BusConfig{Store: st, Runner: runner, Clock: &fakeClock{now: testNow}, Synchronous: true})
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}
	n, err := second.Redrive(ctx)
	if err != nil {
		t.Fatalf("Redrive: %v", err)
	}
	if n != 2 {
		t.Fatalf("redrove %d deliveries, want the request and its reply", n)
	}
	if runner.callCount() != 1 {
		t.Fatalf("runner called %d times after redrive, want 1", runner.callCount())
	}
}

// Two workers must never run the same directive twice. The delivery claim
// is what guarantees it.
func TestConcurrentDeliveryOfOneRowHappensOnce(t *testing.T) {
	b, st, runner, _, _, _ := newTestBus(t)
	ctx := testCtx()

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Request, Content: "once please",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	target := queued[0].ID

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.deliverOne(ctx, target)
		}()
	}
	wg.Wait()

	if runner.callCount() != 1 {
		t.Fatalf("the directive ran %d times, want exactly 1", runner.callCount())
	}
}

func TestStartAndStopAreSafeToCallTwice(t *testing.T) {
	st, runner := newMemStore(), newFakeRunner()
	b, err := NewBus(BusConfig{Store: st, Runner: runner, Options: Options{Workers: 2}})
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}
	ctx := testCtx()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := b.Start(ctx); err != nil {
		t.Fatalf("second Start must be a no-op, got %v", err)
	}
	b.Stop()
	b.Stop()
}

// The workers are the real path: send, then wait for the run to happen
// without the test ever driving delivery itself.
func TestWorkersDeliverWithoutADrainCall(t *testing.T) {
	st, runner := newMemStore(), newFakeRunner()
	done := make(chan struct{})
	var once sync.Once
	runner.respond = func(string, string) string {
		once.Do(func() { close(done) })
		return "answered"
	}

	b, err := NewBus(BusConfig{Store: st, Runner: runner, Options: Options{Workers: 2}})
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}
	ctx := testCtx()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Request, Content: "wake up",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Bounded, because an unbounded wait turns a broken dispatcher into a
	// ten minute hang that reports nothing.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		queued, _ := st.ListQueuedDeliveries(ctx, 10)
		t.Fatalf("the workers never carried the message; %d rows still queued", len(queued))
	}
}

// A store that is momentarily busy must not strand a delivery until the
// next sweep. On sqlite a concurrent write is answered with SQLITE_BUSY,
// which is ordinary rather than exceptional, and a worker that waited a
// full interval on it would make every colliding message arrive half a
// minute late.
func TestATransientStoreErrorIsRetriedPromptly(t *testing.T) {
	st, runner := newMemStore(), newFakeRunner()
	delivered := make(chan struct{})
	var once sync.Once
	runner.respond = func(string, string) string {
		once.Do(func() { close(delivered) })
		return "answered"
	}

	b, err := NewBus(BusConfig{
		Store: st, Runner: runner,
		// A sweep interval far longer than the test's patience, so the
		// only way this passes is the retry.
		Options: Options{Workers: 1, SweepInterval: time.Hour},
	})
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}
	ctx := testCtx()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Stop()

	// The first claim fails, which consumes the wake nudge that the send
	// below produces.
	st.failNextClaims(1)

	if _, err := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Request, Content: "retry me",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("a delivery stranded by one busy claim was never retried")
	}
}

// A process that dies mid-delivery leaves its claim behind. Nothing
// wedges, because an ask resolves on its deadline either way, but an
// informative caught in that window is simply lost unless somebody
// reclaims the row.
func TestStaleClaimsAreReclaimed(t *testing.T) {
	st, runner := newMemStore(), newFakeRunner()
	clk := &fakeClock{now: testNow}
	st.useClock(clk)
	b, err := NewBus(BusConfig{
		Store: st, Runner: runner, Clock: clk, Synchronous: true,
		Options: Options{Workers: 1, DeliveryClaimTTL: 10 * time.Minute},
	})
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}
	ctx := testCtx()

	if _, sendErr := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "do not lose me",
	}); sendErr != nil {
		t.Fatalf("Send: %v", sendErr)
	}

	// A worker claims the row and the process dies before delivering it.
	queued, _ := st.ListQueuedDeliveries(ctx, 10)
	if _, claimErr := st.ClaimDelivery(ctx, queued[0].ID); claimErr != nil {
		t.Fatalf("ClaimDelivery: %v", claimErr)
	}
	if left, _ := st.ListQueuedDeliveries(ctx, 10); len(left) != 0 {
		t.Fatal("the claim should have taken the row out of the queue")
	}

	// Too soon: a delivery legitimately in flight must not be taken away
	// from the worker carrying it.
	clk.advance(time.Minute)
	n, err := b.ReclaimStaleDeliveries(ctx)
	if err != nil {
		t.Fatalf("ReclaimStaleDeliveries: %v", err)
	}
	if n != 0 {
		t.Fatalf("reclaimed %d rows that were still in flight", n)
	}

	// Past the TTL, nobody is coming back for it.
	clk.advance(11 * time.Minute)
	n, err = b.ReclaimStaleDeliveries(ctx)
	if err != nil {
		t.Fatalf("ReclaimStaleDeliveries: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d, want the abandoned row", n)
	}

	// Reclaiming puts it back in the queue rather than delivering it
	// directly, so it takes the ordinary path with its ordinary claim.
	again, _ := st.ListQueuedDeliveries(ctx, 10)
	if len(again) != 1 {
		t.Fatalf("%d rows queued after a reclaim, want 1", len(again))
	}
	if _, drainErr := b.Drain(ctx); drainErr != nil {
		t.Fatalf("Drain: %v", drainErr)
	}
	inbox, _ := st.ListInbox(ctx, "w1", InboxFilter{UnreadOnly: true})
	if len(inbox) != 1 {
		t.Fatalf("the reclaimed message never arrived: %+v", inbox)
	}
}

// A delivered row is finished, and reclaiming it would deliver the same
// message twice.
func TestReclaimLeavesFinishedRowsAlone(t *testing.T) {
	st, runner := newMemStore(), newFakeRunner()
	clk := &fakeClock{now: testNow}
	st.useClock(clk)
	b, err := NewBus(BusConfig{
		Store: st, Runner: runner, Clock: clk, Synchronous: true,
		Options: Options{Workers: 1, DeliveryClaimTTL: time.Minute},
	})
	if err != nil {
		t.Fatalf("NewBus: %v", err)
	}
	ctx := testCtx()

	if _, sendErr := b.Send(ctx, SendParams{
		Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "w1"}},
		Performative: Inform, Content: "already arrived",
	}); sendErr != nil {
		t.Fatalf("Send: %v", sendErr)
	}
	if _, drainErr := b.Drain(ctx); drainErr != nil {
		t.Fatalf("Drain: %v", drainErr)
	}

	clk.advance(time.Hour)
	n, err := b.ReclaimStaleDeliveries(ctx)
	if err != nil {
		t.Fatalf("ReclaimStaleDeliveries: %v", err)
	}
	if n != 0 {
		t.Fatalf("reclaimed %d finished rows, which would deliver them twice", n)
	}
}
