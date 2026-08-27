package a2a

import (
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Envelope validation errors.
var (
	// ErrInvalidPerformative means the envelope names a speech act that is
	// not one of the 22 FIPA-ACL performatives.
	ErrInvalidPerformative = errors.New("cortex: a2a: unknown performative")
	// ErrNoReceivers means the envelope addresses nobody, or addresses an
	// entry with an empty agent name.
	ErrNoReceivers = errors.New("cortex: a2a: envelope has no receivers")
	// ErrNoSender means the envelope carries no sender.
	ErrNoSender = errors.New("cortex: a2a: envelope has no sender")
	// ErrSelfAddressed means the sender is among the receivers. Cortex
	// refuses it outright: it is a loop with no use case behind it.
	ErrSelfAddressed = errors.New("cortex: a2a: envelope is self-addressed")
)

// Address identifies one messaging endpoint. Node empty means an agent in
// this engine; a non-empty Node names a remote peer and is the only field
// remote delivery needs.
type Address struct {
	Agent string `json:"agent"`
	Node  string `json:"node,omitempty"`
}

// IsLocal reports whether the address resolves inside this engine.
func (a Address) IsLocal() bool { return a.Node == "" }

// IsZero reports whether the address names nothing.
func (a Address) IsZero() bool { return a.Agent == "" && a.Node == "" }

// Equal reports whether two addresses name the same endpoint.
func (a Address) Equal(other Address) bool {
	return a.Agent == other.Agent && a.Node == other.Node
}

// String renders the address as agent, or agent@node for a remote peer.
func (a Address) String() string {
	if a.Node == "" {
		return a.Agent
	}
	return a.Agent + "@" + a.Node
}

// Envelope is one FIPA-ACL message. The first block is the ACL parameter
// set, verbatim; the second is cortex's own additions, kept apart from it.
type Envelope struct {
	cortex.Entity
	ID    id.MessageID `json:"id"`
	Scope cortex.Scope `json:"scope"`

	Performative   Performative      `json:"performative"`
	Sender         Address           `json:"sender"`
	Receivers      []Address         `json:"receivers"`
	ReplyTo        []Address         `json:"reply_to,omitempty"`
	Content        string            `json:"content"`
	Language       string            `json:"language,omitempty"`
	Encoding       string            `json:"encoding,omitempty"`
	Ontology       string            `json:"ontology,omitempty"`
	Protocol       string            `json:"protocol,omitempty"`
	ConversationID id.ConversationID `json:"conversation_id"`
	ReplyWith      string            `json:"reply_with,omitempty"`
	InReplyTo      string            `json:"in_reply_to,omitempty"`
	ReplyBy        *time.Time        `json:"reply_by,omitempty"`

	Hops        int            `json:"hops"`
	OriginRunID id.AgentRunID  `json:"origin_run_id,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// Validate checks everything about an envelope that can be decided without
// touching the store: the performative is real, somebody sent it, somebody
// receives it, and the sender is not among the receivers.
func (e *Envelope) Validate() error {
	if !e.Performative.Valid() {
		return ErrInvalidPerformative
	}
	if e.Sender.IsZero() {
		return ErrNoSender
	}
	if len(e.Receivers) == 0 {
		return ErrNoReceivers
	}
	for _, r := range e.Receivers {
		if r.Agent == "" {
			return ErrNoReceivers
		}
		if r.Equal(e.Sender) {
			return ErrSelfAddressed
		}
	}
	return nil
}
