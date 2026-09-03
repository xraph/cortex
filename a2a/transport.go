package a2a

import "context"

// inProcess is the default transport: it handles agents in this engine and
// leaves the work to the bus, which already holds the runner and the store.
// A remote transport does real I/O in Deliver instead.
type inProcess struct{}

func (inProcess) Handles(addr Address) bool { return addr.IsLocal() }

func (inProcess) Deliver(context.Context, *Envelope, Address) error { return nil }

var _ Transport = inProcess{}
