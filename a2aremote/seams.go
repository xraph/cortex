package a2aremote

import (
	"context"
	"crypto/tls"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/skill"
)

// Gateway is the cortex surface the remote service needs. The engine
// satisfies it through a thin adapter, which is what keeps the core
// module from knowing this package exists.
type Gateway interface {
	SendMessage(ctx context.Context, p a2a.SendParams) (*a2a.SendResult, error)
	GetRun(ctx context.Context, runID id.AgentRunID) (*run.Run, error)
	// GetDelivery resolves the handle a peer was given at send time. A
	// task id is a delivery id until the delivery has started a run.
	GetDelivery(ctx context.Context, deliveryID id.DeliveryID) (*a2a.Delivery, error)
	ListRuns(ctx context.Context, filter *run.ListFilter) ([]*run.Run, error)
	CancelRun(ctx context.Context, runID id.AgentRunID) error
	GetAgentByName(ctx context.Context, name string) (*agent.Config, error)
	GetSkillByName(ctx context.Context, name string) (*skill.Skill, error)
}

// Credentials is everything a binding could learn about a caller.
//
// It is transport-neutral on purpose: HTTP headers and gRPC metadata are
// both string-keyed multimaps, and mutual TLS peers arrive through the
// same struct. A resolver written once serves all three bindings.
type Credentials struct {
	Headers    map[string][]string
	RemoteAddr string
	TLS        *tls.ConnectionState
}

// Header returns the first value for a header, case-insensitively.
func (c Credentials) Header(name string) string {
	for k, v := range c.Headers {
		if len(v) > 0 && equalFold(k, name) {
			return v[0]
		}
	}
	return ""
}

// Peer is who a caller turned out to be.
type Peer struct {
	// Node is how this caller appears as an a2a.Address.Node. Every
	// sender name a peer claims is namespaced by it, which is what stops
	// a peer presenting itself as one of your own agents.
	Node string
	// Scope is the cortex scope this peer's messages act in. It is the
	// only place a scope enters an inbound request.
	Scope cortex.Scope
}

// PeerResolver authenticates an inbound caller.
//
// Cortex ships the seam and no implementation: hosts already have an
// identity system, and a second weaker one living inside cortex would be
// a liability rather than a convenience. Returning an error refuses the
// request, and the caller is told only that it was refused.
type PeerResolver interface {
	ResolvePeer(ctx context.Context, cred Credentials) (Peer, error)
}

// ResolverFunc adapts a function to PeerResolver.
type ResolverFunc func(ctx context.Context, cred Credentials) (Peer, error)

// ResolvePeer implements PeerResolver.
func (f ResolverFunc) ResolvePeer(ctx context.Context, cred Credentials) (Peer, error) {
	return f(ctx, cred)
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		if lower(a[i]) != lower(b[i]) {
			return false
		}
	}
	return true
}

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}
