package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	// ErrUnknownReceiver means the address is routable in shape but names
	// no agent the sender can reach.
	ErrUnknownReceiver = errors.New("cortex: a2a: receiver does not resolve")
)

// BusConfig builds a Bus. Store and Runner are required; everything else
// has a working default.
type BusConfig struct {
	Store      Store
	Runner     cortex.AgentRunner
	Resumer    Resumer
	Resolver   Resolver
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
	store    Store
	runner   cortex.AgentRunner
	resumer  Resumer
	resolver Resolver
	hooks    HookEmitter
	clock    Clock
	opts     Options
	dispatch *dispatcher

	// mu guards transports, which AddTransport appends to after the bus
	// exists. Registration happens at startup and reads happen once per
	// delivery, so a plain mutex is the right weight here.
	mu         sync.RWMutex
	transports []Transport
}

// AddTransport registers a transport after the bus has been built.
//
// It exists because construction cannot be circular. A remote transport
// needs the bus, since a peer's reply is fed back through Send so it can
// resolve a waiting ask, and the bus needs the transport to carry the
// question out. The host builds the bus, builds the transport with it,
// and registers it here.
func (b *Bus) AddTransport(t Transport) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transports = append(b.transports, t)
}

// transportFor returns the transport that claims addr, or nil.
func (b *Bus) transportFor(addr Address) Transport {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, t := range b.transports {
		if t.Handles(addr) {
			return t
		}
	}
	return nil
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
		resolver:   cfg.Resolver,
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
		if resolveErr := b.resolve(ctx, r); resolveErr != nil {
			return nil, nil, resolveErr
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

	// A reply that answers a waiting ask reaches its asker through the
	// resume, so queueing a delivery as well would hand the same agent the
	// same words twice.
	resumed, err := b.resolveAsk(ctx, e)
	if err != nil {
		return nil, err
	}
	if resumed {
		b.hooks.MessageSent(ctx, e.ID, e.Sender.String(), addressList(e.Receivers), string(e.Performative))
		return res, nil
	}

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

// resolve asks the host whether the receiver exists. A bus with no
// resolver skips the question, which is what the package's own tests do:
// they have no agents, only a fake runner that answers to any name.
func (b *Bus) resolve(ctx context.Context, addr Address) error {
	if b.resolver == nil {
		return nil
	}
	if err := b.resolver.ResolveAddress(ctx, addr); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnknownReceiver, addr, err)
	}
	return nil
}

func (b *Bus) routable(addr Address) bool { return b.transportFor(addr) != nil }

func addressList(addrs []Address) string {
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = a.String()
	}
	return strings.Join(out, ",")
}

// Ask errors.
var (
	// ErrAskNeedsOneReceiver means Ask was given zero or several receivers.
	// A durable ask correlates one reply to one waiting run.
	ErrAskNeedsOneReceiver = errors.New("cortex: a2a: ask needs exactly one receiver")
	// ErrAskNeedsDirective means the performative does not demand an answer,
	// so nothing would ever resume the asker.
	ErrAskNeedsDirective = errors.New("cortex: a2a: ask needs a directive performative")
)

// AskParams is one message whose sender suspends until the answer arrives.
type AskParams struct {
	SendParams
	AskerRunID id.AgentRunID
	ToolCallID string
}

// AskResult identifies the message and the token a reply must carry.
type AskResult struct {
	MessageID      id.MessageID      `json:"message_id"`
	ConversationID id.ConversationID `json:"conversation_id"`
	ReplyWith      string            `json:"reply_with"`
}

// Ask sends a directive and records the sender's run as waiting on the
// answer. The caller suspends its run once this returns.
//
// The ledger row is written AFTER the message, and the whole thing is
// refused before either write when the send could not go out. A pending
// ask with no message behind it is a run nothing could ever resume.
func (b *Bus) Ask(ctx context.Context, p AskParams) (*AskResult, error) {
	if p.Performative == "" {
		p.Performative = Request
	}
	if len(p.Receivers) != 1 {
		return nil, ErrAskNeedsOneReceiver
	}
	if c, ok := p.Performative.Class(); !ok || c != ClassDirective {
		return nil, ErrAskNeedsDirective
	}
	if p.ReplyWith == "" {
		p.ReplyWith = id.NewMessageID().String()
	}
	if p.ReplyBy == nil {
		by := b.clock.Now().Add(b.opts.DefaultReplyBy)
		p.ReplyBy = &by
	}

	e, conv, err := b.prepare(ctx, p.SendParams)
	if err != nil {
		return nil, err
	}
	sent, err := b.submit(ctx, e, conv)
	if err != nil {
		return nil, err
	}

	ask := &PendingAsk{
		Entity:         cortex.NewEntity(),
		Scope:          e.Scope,
		ReplyWith:      e.ReplyWith,
		ConversationID: e.ConversationID,
		MessageID:      e.ID,
		AskerRunID:     p.AskerRunID,
		AskerAgent:     e.Sender.Agent,
		ToolCallID:     p.ToolCallID,
		Expected:       e.Receivers[0],
		Deadline:       e.ReplyBy,
	}
	if err := b.store.CreatePendingAsk(ctx, ask); err != nil {
		return nil, err
	}
	return &AskResult{MessageID: sent.MessageID, ConversationID: sent.ConversationID, ReplyWith: e.ReplyWith}, nil
}

// AskReply is what a resumed agent_ask tool call returns to the model.
type AskReply struct {
	Performative   string `json:"performative"`
	Sender         string `json:"sender"`
	Content        string `json:"content"`
	ConversationID string `json:"conversation_id"`
}

// resolveAsk matches an inbound reply to a waiting ask and resumes it,
// reporting whether a run was resumed.
//
// The claim happens before the resume, and that ordering is the design
// rather than a precaution: a late reply, the deadline sweep and a cancel
// are three writers racing for one row, and only the winner may resume.
func (b *Bus) resolveAsk(ctx context.Context, e *Envelope) (bool, error) {
	if e.InReplyTo == "" || !e.Performative.ResolvesAsk() {
		return false, nil
	}
	ask, err := b.store.ClaimPendingAsk(ctx, e.InReplyTo)
	switch {
	case errors.Is(err, ErrAskNotFound), errors.Is(err, ErrAskAlreadyClaimed):
		return false, nil
	case err != nil:
		return false, err
	}
	if b.resumer == nil {
		return false, nil
	}

	payload, err := json.Marshal(AskReply{
		Performative:   string(e.Performative),
		Sender:         e.Sender.String(),
		Content:        e.Content,
		ConversationID: e.ConversationID.String(),
	})
	if err != nil {
		return false, err
	}
	if err := b.resumer.ResumeAgentReply(ctx, ask.AskerRunID, ask.ToolCallID, string(payload)); err != nil {
		return false, err
	}
	return true, nil
}

// InboxItem is one delivered message as an agent sees it.
type InboxItem struct {
	DeliveryID     string `json:"delivery_id"`
	MessageID      string `json:"message_id"`
	ConversationID string `json:"conversation_id"`
	Sender         string `json:"sender"`
	Performative   string `json:"performative"`
	Content        string `json:"content"`
	ReceivedAt     string `json:"received_at,omitempty"`
}

// Inbox returns delivered messages for an agent and marks what it returns
// as read. Reading is the acknowledgement: an agent that already saw a
// message in a tool result must not be handed it again next turn.
func (b *Bus) Inbox(ctx context.Context, agentName string, f InboxFilter) ([]InboxItem, error) {
	rows, err := b.store.ListInbox(ctx, agentName, f)
	if err != nil {
		return nil, err
	}
	items := make([]InboxItem, 0, len(rows))
	for _, d := range rows {
		e, err := b.store.GetMessage(ctx, d.MessageID)
		if err != nil {
			return nil, err
		}
		item := InboxItem{
			DeliveryID:     d.ID.String(),
			MessageID:      e.ID.String(),
			ConversationID: e.ConversationID.String(),
			Sender:         e.Sender.String(),
			Performative:   string(e.Performative),
			Content:        e.Content,
		}
		if d.DeliveredAt != nil {
			item.ReceivedAt = d.DeliveredAt.Format(time.RFC3339)
		}
		items = append(items, item)
		if err := b.store.MarkDeliveryRead(ctx, d.ID); err != nil {
			return nil, err
		}
	}
	return items, nil
}
