package a2a

import "time"

// Defaults for Options. The hop ceiling is the containment budget: eight
// derived messages is generous for a real delegation chain and short of
// anything that reads as a runaway.
const (
	DefaultHopCeiling    = 8
	DefaultWorkers       = 4
	DefaultReplyBy       = 5 * time.Minute
	DefaultSweepInterval = 30 * time.Second
	// DefaultDeliveryClaimTTL is how long a claimed delivery may sit
	// before it is assumed abandoned.
	//
	// It is generous on purpose. A remote delivery legitimately holds its
	// claim while the peer is polled, which the client bounds at two
	// minutes, and reclaiming a delivery somebody is still carrying would
	// deliver the same message twice. Fifteen minutes is well past any
	// honest delivery and well short of a human noticing.
	DefaultDeliveryClaimTTL = 15 * time.Minute
)

// Options tunes the messaging subsystem.
type Options struct {
	// HopCeiling caps how many messages one conversation may derive.
	HopCeiling int
	// Workers is the dispatcher's concurrency. It has to cover resumed
	// askers as well as recipients, because a resume runs synchronously on
	// the worker that delivered the reply.
	Workers int
	// DefaultReplyBy is the deadline stamped on an ask that names none.
	DefaultReplyBy time.Duration
	// SweepInterval is how often overdue asks are resolved into failures,
	// and how often abandoned deliveries are reclaimed.
	SweepInterval time.Duration
	// DeliveryClaimTTL is how long a claimed delivery may sit before it
	// is treated as abandoned by a process that died mid-delivery.
	DeliveryClaimTTL time.Duration
}

func (o Options) withDefaults() Options {
	if o.HopCeiling <= 0 {
		o.HopCeiling = DefaultHopCeiling
	}
	if o.Workers <= 0 {
		o.Workers = DefaultWorkers
	}
	if o.DefaultReplyBy <= 0 {
		o.DefaultReplyBy = DefaultReplyBy
	}
	if o.SweepInterval <= 0 {
		o.SweepInterval = DefaultSweepInterval
	}
	if o.DeliveryClaimTTL <= 0 {
		o.DeliveryClaimTTL = DefaultDeliveryClaimTTL
	}
	return o
}
