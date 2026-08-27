package a2a

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/xraph/cortex/id"
)

// drainBatch caps one pass over the queue.
const drainBatch = 100

// dispatcher carries queued deliveries to the bus. In synchronous mode it
// delivers nothing on its own and waits for Drain, which is what lets a
// test assert without ever observing a run mid-flight.
type dispatcher struct {
	bus         *Bus
	synchronous bool
	wake        chan struct{}

	mu      sync.Mutex
	started bool
	done    chan struct{}
	cancel  context.CancelFunc
}

func newDispatcher(b *Bus, synchronous bool) *dispatcher {
	return &dispatcher{bus: b, synchronous: synchronous, wake: make(chan struct{}, 1)}
}

// enqueue nudges the workers. The queue itself is the store, so a nudge
// that is dropped costs a delay and never a delivery: the next wake, the
// next interval, or a redrive finds the row.
func (d *dispatcher) enqueue(id.DeliveryID) {
	if d.synchronous {
		return
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Start launches the delivery workers. Calling it twice is a no-op, and in
// synchronous mode it starts nothing at all.
func (b *Bus) Start(ctx context.Context) error {
	d := b.dispatch
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started || d.synchronous {
		d.started = true
		return nil
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// done is captured locally rather than read back off the struct when
	// the workers finish. Stop clears the field as soon as it has the
	// handle it needs, so a closer reading it later closes a nil channel.
	done := make(chan struct{})
	d.cancel, d.done, d.started = cancel, done, true

	var wg sync.WaitGroup
	for i := 0; i < b.opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.work(runCtx)
		}()
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	return nil
}

// Stop cancels the workers and WAITS for them. Signalling without waiting
// would let Stop return while a delivery was still writing.
func (b *Bus) Stop() {
	d := b.dispatch
	d.mu.Lock()
	cancel, done := d.cancel, d.done
	d.started, d.cancel, d.done = false, nil, nil
	d.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	<-done
}

func (d *dispatcher) work(ctx context.Context) {
	ticker := time.NewTicker(d.bus.opts.SweepInterval)
	defer ticker.Stop()
	for {
		// A drain error is per batch and the loop keeps going: one bad row
		// must not stop delivery for everyone else.
		_, _ = d.bus.Drain(ctx)

		select {
		case <-ctx.Done():
			return
		case <-d.wake:
		case <-ticker.C:
		}
	}
}

// Drain delivers everything currently queued and reports how many rows it
// carried. Tests call it directly; workers call it in a loop.
//
// The count includes replies the directives produced, because a reply is
// itself a queued delivery and a drain that left them behind would stop
// halfway through the conversation it just started.
func (b *Bus) Drain(ctx context.Context) (int, error) {
	var n int
	for {
		rows, err := b.store.ListQueuedDeliveries(ctx, drainBatch)
		if err != nil {
			return n, err
		}
		if len(rows) == 0 {
			return n, nil
		}
		for _, row := range rows {
			err := b.deliverOne(ctx, row.ID)
			switch {
			case errors.Is(err, ErrDeliveryAlreadyClaimed):
				continue
			case err != nil:
				return n, err
			}
			n++
		}
	}
}

// Redrive picks up deliveries a previous process queued and never carried.
// It is the work Drain does; the separate name is for the caller that runs
// it once at startup.
func (b *Bus) Redrive(ctx context.Context) (int, error) { return b.Drain(ctx) }
