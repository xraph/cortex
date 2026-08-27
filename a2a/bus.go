package a2a

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Bus errors.
var (
	// ErrNoStore means NewBus was called without persistence.
	ErrNoStore = errors.New("cortex: a2a: no store configured")
	// ErrNoRunner means NewBus was called without an agent runner.
	ErrNoRunner = errors.New("cortex: a2a: no agent runner configured")
	// ErrConversationClosed means the conversation no longer accepts messages.
	ErrConversationClosed = errors.New("cortex: a2a: conversation is closed")
	// ErrHopCeiling means the conversation used up its containment budget.
	ErrHopCeiling = errors.New("cortex: a2a: conversation hop ceiling exceeded")
	// ErrUnroutable means no transport handles the receiver's address.
	ErrUnroutable = errors.New("cortex: a2a: no transport handles that address")
)

// BusConfig builds a Bus. Store and Runner are required; everything else
// has a working default.
type BusConfig struct {
	Store      Store
	Runner     cortex.AgentRunner
	Resumer    Resumer
	Hooks      HookEmitter
	Clock      Clock
	Transports []Transport
	Options    Options

	// Synchronous makes the dispatcher deliver only when Drain is called.
	// Tests set it so an assertion can never observe a run mid-flight.
	Synchronous bool
}

// Bus routes envelopes between agents.
type Bus struct {
	store      Store
	runner     cortex.AgentRunner
	resumer    Resumer
	hooks      HookEmitter
	clock      Clock
	transports []Transport
	opts       Options
	dispatch   *dispatcher
}

// NewBus builds a Bus from cfg.
func NewBus(cfg BusConfig) (*Bus, error) {
	if cfg.Store == nil {
		return nil, ErrNoStore
	}
	if cfg.Runner == nil {
		return nil, ErrNoRunner
	}
	if cfg.Hooks == nil {
		cfg.Hooks = noopHooks{}
	}
	if cfg.Clock == nil {
		cfg.Clock = systemClock{}
	}
	b := &Bus{
		store:      cfg.Store,
		runner:     cfg.Runner,
		resumer:    cfg.Resumer,
		hooks:      cfg.Hooks,
		clock:      cfg.Clock,
		transports: cfg.Transports,
		opts:       cfg.Options.withDefaults(),
	}
	if len(b.transports) == 0 {
		b.transports = []Transport{inProcess{}}
	}
	b.dispatch = newDispatcher(b, cfg.Synchronous)
	return b, nil
}

// SendParams is one outbound message.
type SendParams struct {
	Sender         Address
	Receivers      []Address
	Performative   Performative
	Content        string
	ConversationID id.ConversationID
	ReplyTo        []Address
	Language       string
	Encoding       string
	Ontology       string
	Protocol       string
	ReplyWith      string
	InReplyTo      string
	ReplyBy        *time.Time
	OriginRunID    id.AgentRunID
	Metadata       map[string]any
}

// DeliveryOutcome is one receiver's result from a send. A broadcast reports
// per receiver, because failing the whole send over one bad name throws
// away the deliveries that were fine.
type DeliveryOutcome struct {
	Receiver Address `json:"receiver"`
	Status   string  `json:"status"`
	Error    string  `json:"error,omitempty"`
}

// SendResult is what a send produced.
type SendResult struct {
	MessageID      id.MessageID      `json:"message_id"`
	ConversationID id.ConversationID `json:"conversation_id"`
	Deliveries     []DeliveryOutcome `json:"deliveries"`
}

// Send validates, persists and queues one message. It returns as soon as
// the deliveries are queued; nothing waits for the recipients.
func (b *Bus) Send(ctx context.Context, p SendParams) (*SendResult, error) {
	e, conv, err := b.prepare(ctx, p)
	if err != nil {
		return nil, err
	}
	return b.submit(ctx, e, conv)
}

// prepare builds and validates the envelope and resolves its conversation.
// Everything that can refuse a send happens here, before submit writes the
// message: a half-written send must never become a suspension nothing can
// resume.
func (b *Bus) prepare(ctx context.Context, p SendParams) (*Envelope, *Conversation, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, nil, cortex.ErrNoScope
	}

	e := &Envelope{
		Entity:       cortex.NewEntity(),
		ID:           id.NewMessageID(),
		Scope:        scope,
		Performative: p.Performative,
		Sender:       p.Sender,
		Receivers:    p.Receivers,
		ReplyTo:      p.ReplyTo,
		Content:      p.Content,
		Language:     p.Language,
		Encoding:     p.Encoding,
		Ontology:     p.Ontology,
		Protocol:     p.Protocol,
		ReplyWith:    p.ReplyWith,
		InReplyTo:    p.InReplyTo,
		ReplyBy:      p.ReplyBy,
		OriginRunID:  p.OriginRunID,
		Metadata:     p.Metadata,
	}
	if err := e.Validate(); err != nil {
		return nil, nil, err
	}
	for _, r := range e.Receivers {
		if !b.routable(r) {
			return nil, nil, fmt.Errorf("%w: %s", ErrUnroutable, r)
		}
	}

	conv, err := b.resolveConversation(ctx, p, scope)
	if err != nil {
		return nil, nil, err
	}
	if !conv.IsOpen() {
		return nil, nil, ErrConversationClosed
	}
	if conv.HopsUsed+1 > conv.HopCeiling {
		return nil, nil, fmt.Errorf("%w: used %d of %d", ErrHopCeiling, conv.HopsUsed, conv.HopCeiling)
	}

	e.ConversationID = conv.ID
	e.Hops = conv.HopsUsed + 1
	return e, conv, nil
}

func (b *Bus) resolveConversation(ctx context.Context, p SendParams, scope cortex.Scope) (*Conversation, error) {
	if !p.ConversationID.IsNil() {
		return b.store.GetConversation(ctx, p.ConversationID)
	}
	conv := &Conversation{
		Entity:     cortex.NewEntity(),
		ID:         id.NewConversationID(),
		Scope:      scope,
		Protocol:   p.Protocol,
		Initiator:  p.Sender,
		Status:     StatusOpen,
		HopCeiling: b.opts.HopCeiling,
	}
	if err := b.store.CreateConversation(ctx, conv); err != nil {
		return nil, err
	}
	return conv, nil
}

// submit writes the envelope, bumps the conversation, queues one delivery
// per receiver and fires MessageSent. Ordering matters: the envelope lands
// first, so a delivery can never point at a message that is not there.
func (b *Bus) submit(ctx context.Context, e *Envelope, conv *Conversation) (*SendResult, error) {
	if err := b.store.CreateMessage(ctx, e); err != nil {
		return nil, err
	}

	conv.HopsUsed = e.Hops
	conv.AddParticipant(e.Sender)
	for _, r := range e.Receivers {
		conv.AddParticipant(r)
	}
	if err := b.store.UpdateConversation(ctx, conv); err != nil {
		return nil, err
	}

	res := &SendResult{MessageID: e.ID, ConversationID: e.ConversationID}
	for _, r := range e.Receivers {
		d := &Delivery{
			Entity:    cortex.NewEntity(),
			ID:        id.NewDeliveryID(),
			Scope:     e.Scope,
			MessageID: e.ID,
			Receiver:  r,
			State:     DeliveryQueued,
		}
		if err := b.store.CreateDelivery(ctx, d); err != nil {
			res.Deliveries = append(res.Deliveries, DeliveryOutcome{Receiver: r, Status: DeliveryFailed, Error: err.Error()})
			continue
		}
		res.Deliveries = append(res.Deliveries, DeliveryOutcome{Receiver: r, Status: DeliveryQueued})
		b.dispatch.enqueue(d.ID)
	}

	b.hooks.MessageSent(ctx, e.ID, e.Sender.String(), addressList(e.Receivers), string(e.Performative))
	return res, nil
}

func (b *Bus) routable(addr Address) bool {
	for _, t := range b.transports {
		if t.Handles(addr) {
			return true
		}
	}
	return false
}

func addressList(addrs []Address) string {
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = a.String()
	}
	return strings.Join(out, ",")
}
