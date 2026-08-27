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
	// SweepInterval is how often overdue asks are resolved into failures.
	SweepInterval time.Duration
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
	return o
}
