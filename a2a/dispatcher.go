package a2a

import "github.com/xraph/cortex/id"

// dispatcher carries queued deliveries to the bus. Task 13 gives it real
// workers; for now it only records the synchronous mode.
type dispatcher struct {
	bus         *Bus
	synchronous bool
}

func newDispatcher(b *Bus, synchronous bool) *dispatcher {
	return &dispatcher{bus: b, synchronous: synchronous}
}

func (d *dispatcher) enqueue(id.DeliveryID) {}
