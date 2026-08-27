package a2a

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
)

// Clock is the package's only source of time. Everything reads it, so a
// test can move a deadline without sleeping.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Resumer un-pauses a run that stopped on an agent_ask. It wraps the
// engine's internal resume path, not the public Resume, because a host
// must not be able to forge a peer's reply.
type Resumer interface {
	ResumeAgentReply(ctx context.Context, runID id.AgentRunID, callID, result string) error
}

// Transport delivers an envelope to one receiver. The in-process
// implementation resolves agents in this engine; a remote implementation
// resolves a Node.
type Transport interface {
	// Deliver hands the envelope to the receiver. Returning an error marks
	// the delivery failed; it does not fail the sender's run.
	Deliver(ctx context.Context, e *Envelope, receiver Address) error
	// Handles reports whether this transport can reach the address.
	Handles(addr Address) bool
}

// HookEmitter receives messaging lifecycle events. The engine adapts
// plugin.Registry to it; tests pass a recorder.
type HookEmitter interface {
	MessageSent(ctx context.Context, msgID id.MessageID, from, to, performative string)
	MessageDelivered(ctx context.Context, msgID id.MessageID, to string)
	MessageRefused(ctx context.Context, msgID id.MessageID, to, reason string)
}

type noopHooks struct{}

func (noopHooks) MessageSent(context.Context, id.MessageID, string, string, string) {}
func (noopHooks) MessageDelivered(context.Context, id.MessageID, string)            {}
func (noopHooks) MessageRefused(context.Context, id.MessageID, string, string)      {}
