package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"

	gometrics "github.com/xraph/go-utils/metrics"

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

// BusConfig builds a Bus. Store and one runner are required. Structured
// handoffs require HandoffHandler explicitly.
type BusConfig struct {
	// Roster restricts a non-global bus to captured local participant identities.
	Roster *Roster
	// ReservedRunner replaces Runner for partitioned execution with durable admission.
	ReservedRunner cortex.ReservedAgentRunner
	// OrchestrationID fixes this bus to one routing partition. Zero is global.
	OrchestrationID id.OrchestrationID
	// HandoffHandler is required for structured assignments. No ordinary agent
	// fallback is permitted. The host owns current authority and durable intake.
	HandoffHandler HandoffHandler
	Store          Store
	Runner         cortex.AgentRunner
	Resumer        Resumer
	Resolver       Resolver
	Hooks          HookEmitter
	Clock          Clock
	Transports     []Transport
	Options        Options

	// Metrics records the bus's delivery counter. Nil, the default,
	// records nothing; the engine passes whatever WithMetrics gave it.
	Metrics gometrics.Metrics

	// Synchronous makes the dispatcher deliver only when Drain is called.
	// Tests set it so an assertion can never observe a run mid-flight.
	Synchronous bool
}

// Bus routes envelopes between agents.
type Bus struct {
	resolutions     AskResolutionStore
	recorder        AtomicMessageStore
	replyCommitter  ReservedReplyStore
	roster          *frozenRoster
	reserved        cortex.ReservedAgentRunner
	binder          DeliveryRunBinder
	orchestrationID id.OrchestrationID
	handoffHandler  HandoffHandler
	store           Store
	runner          cortex.AgentRunner
	resumer         Resumer
	resolver        Resolver
	hooks           HookEmitter
	clock           Clock
	opts            Options
	dispatch        *dispatcher
	metrics         busMetrics

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
	roster, rosterErr := freezeRoster(cfg.Roster, cfg.OrchestrationID)
	if rosterErr != nil {
		return nil, rosterErr
	}
	if cfg.Store == nil {
		return nil, ErrNoStore
	}
	if cfg.Runner == nil && cfg.ReservedRunner == nil {
		return nil, ErrNoRunner
	}
	if cfg.ReservedRunner != nil {
		if cfg.Runner != nil || cfg.OrchestrationID.IsNil() {
			return nil, cortex.ErrInvalidPrepared
		}
		if _, supported := deliveryRunBinder(cfg.Store); !supported {
			return nil, ErrDeliveryBindingUnavailable
		}
	}
	backend := cfg.Store
	if bound, ok := backend.(*routingStore); ok {
		backend = bound.inner
	}
	_, atomicMessages := backend.(AtomicMessageStore)
	_, atomicReplies := backend.(ReservedReplyStore)
	_, atomicResolutions := backend.(AskResolutionStore)
	if cfg.ReservedRunner != nil && !atomicReplies {
		return nil, ErrReplyCommitUnavailable
	}
	if cfg.Hooks == nil {
		cfg.Hooks = noopHooks{}
	}
	if cfg.Clock == nil {
		cfg.Clock = systemClock{}
	}
	routing, routingErr := newBusRoutingStore(cfg.Store, cfg.OrchestrationID, roster)
	if routingErr != nil {
		return nil, routingErr
	}
	b := &Bus{
		roster:          roster,
		reserved:        cfg.ReservedRunner,
		orchestrationID: cfg.OrchestrationID,
		handoffHandler:  cfg.HandoffHandler,
		store:           routing,
		runner:          cfg.Runner,
		resumer:         cfg.Resumer,
		resolver:        cfg.Resolver,
		hooks:           cfg.Hooks,
		clock:           cfg.Clock,
		transports:      cfg.Transports,
		opts:            cfg.Options.withDefaults(),
		metrics:         newBusMetrics(cfg.Metrics),
	}
	if cfg.ReservedRunner != nil {
		var supported bool
		b.binder, supported = routing.(DeliveryRunBinder)
		if !supported {
			return nil, ErrDeliveryBindingUnavailable
		}
		b.replyCommitter, supported = routing.(ReservedReplyStore)
		if !supported {
			return nil, ErrReplyCommitUnavailable
		}
	}
	if atomicResolutions {
		var supported bool
		b.resolutions, supported = routing.(AskResolutionStore)
		if !supported {
			return nil, ErrAskResolutionUnavailable
		}
	}
	if atomicMessages {
		var supported bool
		b.recorder, supported = routing.(AtomicMessageStore)
		if !supported {
			return nil, ErrReplyCommitUnavailable
		}
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

	// PeerNode and PeerContext name a remote thread this message
	// belongs to, for a message arriving from another engine. When they
	// are set and no conversation id is, the send joins the conversation
	// opened for that remote thread, or opens one and records the
	// pairing.
	PeerNode    string
	PeerContext string
}

// DeliveryOutcome is one receiver's result from a send. A broadcast reports
// per receiver, because failing the whole send over one bad name throws
// away the deliveries that were fine.
type DeliveryOutcome struct {
	Receiver Address `json:"receiver"`
	Status   string  `json:"status"`
	Error    string  `json:"error,omitempty"`
	// DeliveryID is the row this delivery became.
	//
	// It exists because a caller often needs a handle to the work before
	// the work exists: at send time nothing has been delivered, so there
	// is no run to name yet, and the delivery is the only durable thing
	// to point at. The remote transport hands it to a peer as a task id.
	DeliveryID id.DeliveryID `json:"delivery_id,omitempty"`
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
		Receivers:    slices.Clone(p.Receivers),
		ReplyTo:      slices.Clone(p.ReplyTo),
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
	if b.roster != nil && (p.PeerNode != "" || p.PeerContext != "") {
		return nil, nil, ErrRosterDenied
	}
	if err := b.checkRosterEnvelope(ctx, e); err != nil {
		return nil, nil, err
	}
	if handoffErr := b.handoff(ctx, e); handoffErr != nil {
		return nil, nil, handoffErr
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

	// A message from a peer continues that peer's thread when we already
	// opened one for it. Starting fresh instead would hand the peer a
	// new hop budget every turn.
	if p.PeerNode != "" && p.PeerContext != "" {
		existing, err := b.store.GetConversationByPeerContext(ctx, p.PeerNode, p.PeerContext)
		switch {
		case err == nil:
			return existing, nil
		case !errors.Is(err, ErrConversationNotFound):
			return nil, err
		}
	}

	conv := &Conversation{
		Entity:      cortex.NewEntity(),
		ID:          id.NewConversationID(),
		Scope:       scope,
		Protocol:    p.Protocol,
		Initiator:   p.Sender,
		Status:      StatusOpen,
		HopCeiling:  b.opts.HopCeiling,
		PeerNode:    p.PeerNode,
		PeerContext: p.PeerContext,
	}
	if err := b.store.CreateConversation(ctx, conv); err != nil {
		return nil, err
	}
	return conv, nil
}

// submit records the envelope and routes it. Ordering matters: the
// envelope lands first, so a delivery can never point at a message that is
// not there.
func (b *Bus) submit(ctx context.Context, e *Envelope, conv *Conversation) (*SendResult, error) {
	if err := b.record(ctx, e, conv); err != nil {
		return nil, err
	}
	return b.route(ctx, e)
}

// record writes the envelope and bumps the conversation. Nothing it does
// can wake anybody: a recorded message that was never routed is a line in
// the transcript and nothing more.
func (b *Bus) record(ctx context.Context, e *Envelope, conv *Conversation) error {
	if b.recorder != nil {
		committed, err := b.recorder.RecordMessage(ctx, e)
		if err != nil {
			return err
		}
		if committed == nil {
			return ErrReplyCommitConflict
		}
		*e = *committed
		return nil
	}
	if err := b.store.CreateMessage(ctx, e); err != nil {
		return err
	}

	conv.HopsUsed = e.Hops
	conv.AddParticipant(e.Sender)
	for _, r := range e.Receivers {
		conv.AddParticipant(r)
	}
	return b.store.UpdateConversation(ctx, conv)
}

// route makes a recorded envelope act: it answers the ask it replies to,
// or queues one delivery per receiver, and fires MessageSent. This is the
// step that can set another run going, so it is the one Ask holds back
// until the asker is ready to hear the answer.
func (b *Bus) route(ctx context.Context, e *Envelope) (*SendResult, error) {
	if err := b.checkRosterEnvelope(ctx, e); err != nil {
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
		res.Deliveries = append(res.Deliveries, DeliveryOutcome{Receiver: r, Status: DeliveryQueued, DeliveryID: d.ID})
		b.dispatch.links.put(d.ID, trace.SpanContextFromContext(ctx), b.clock.Now())
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
	// ErrAskNeedsReceivers means Ask was given nobody to ask.
	ErrAskNeedsReceivers = errors.New("cortex: a2a: ask needs at least one receiver")
	// ErrAskNeedsDirective means the performative does not demand an answer,
	// so nothing would ever resume the asker.
	ErrAskNeedsDirective = errors.New("cortex: a2a: ask needs a directive performative")
)

// AskParams is one message whose sender suspends until the answer arrives.
type AskParams struct {
	SendParams
	AskerRunID id.AgentRunID
	ToolCallID string

	// Hold writes the message and the pending ask but routes nothing.
	// The question is not queued, and no ask it answers is resumed, until
	// Release is called with the result.
	//
	// A caller that suspends its run after Ask returns needs this. Routed
	// at once, the question can be carried, answered and replied to
	// before the asker has paused, and a reply that finds no paused run
	// has nothing to resume: the asker then waits on an answer that has
	// already come and gone.
	Hold bool
}

// AskResult identifies the message and the token a reply must carry.
type AskResult struct {
	MessageID      id.MessageID      `json:"message_id"`
	ConversationID id.ConversationID `json:"conversation_id"`
	ReplyWith      string            `json:"reply_with"`

	// held is the envelope Release routes. It is nil for an ask that was
	// routed when it was sent, and for one that has been released.
	held *Envelope
}

// Ask sends a directive and records the sender's run as waiting on the
// answer. The caller suspends its run once this returns.
//
// The writes go message, then pending ask, then routing, and both halves
// of that order are load-bearing. A pending ask with no message behind it
// is a run nothing could ever resume. A question routed before its pending
// ask exists can be answered before there is anything for the answer to
// claim, and the reply then queues as an ordinary message while the asker
// waits out its deadline.
//
// Everything that can refuse the send happens before the first write.
func (b *Bus) Ask(ctx context.Context, p AskParams) (*AskResult, error) {
	if p.Performative == "" {
		p.Performative = Request
	}
	if len(p.Receivers) == 0 {
		return nil, ErrAskNeedsReceivers
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
	if err := b.record(ctx, e, conv); err != nil {
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

	res := &AskResult{MessageID: e.ID, ConversationID: e.ConversationID, ReplyWith: e.ReplyWith}
	if p.Hold {
		res.held = e
		return res, nil
	}
	if _, err := b.route(ctx, e); err != nil {
		// The caller hears the ask was refused and will not suspend, so
		// the ask must not be left waiting. Claimed, it is out of the
		// deadline sweep's reach, and nothing tries to resume a run that
		// never paused. Best effort: the error the caller needs is the
		// one that stopped the send.
		_, _ = b.store.ClaimPendingAsk(ctx, e.ReplyWith) //nolint:errcheck // best-effort retirement of a refused ask
		return nil, err
	}
	return res, nil
}

// Release routes an ask that was sent with Hold. Calling it on an ask that
// was routed when it was sent, or calling it twice, does nothing: queueing
// the deliveries again would carry the question twice.
//
// The caller releases once the asker has paused. A release that fails
// leaves the ask waiting with nothing carrying the question, and the
// deadline sweep resolves it into a failure the asking agent can read.
func (b *Bus) Release(ctx context.Context, res *AskResult) error {
	if res == nil || res.held == nil {
		return nil
	}
	e := res.held
	res.held = nil
	_, err := b.route(ctx, e)
	return err
}

// AskReply is what a resumed agent_ask tool call returns to the model.
//
// Replies is plural because an ask may have gone to several agents at
// once, which is what a call for proposals is. A single-recipient ask
// carries exactly one entry, so the payload never changes shape with the
// number of recipients: an agent reads the same structure either way.
type AskReply struct {
	Replies []AskReplyItem `json:"replies"`
	// Complete says every recipient answered. False means the deadline
	// arrived first and Replies holds whoever did, which is an ordinary
	// outcome for a tender rather than a failure.
	Complete bool `json:"complete"`
}

// AskReplyItem is one agent's answer.
type AskReplyItem struct {
	Performative   string `json:"performative"`
	Sender         string `json:"sender"`
	Content        string `json:"content"`
	ConversationID string `json:"conversation_id"`
}

// resolveAsk matches an inbound reply to a waiting ask and resumes it
// once the ask has everything it was waiting for, reporting whether a run
// was resumed.
//
// An ask addressed to one agent resumes on that agent's answer. An ask
// addressed to several waits for the whole field, because an initiator
// that resumed on the first proposal would be choosing before the others
// had spoken, which is the opposite of what a tender is for.
//
// The claim happens before the resume, and that ordering is the design
// rather than a precaution: a late reply, the deadline sweep and a cancel
// are three writers racing for one row, and only the winner may resume.
func (b *Bus) resolveAsk(ctx context.Context, e *Envelope) (bool, error) {
	if e.InReplyTo == "" || !e.Performative.AnswersAsk() {
		return false, nil
	}

	pending, ready, replies, err := b.tallyAnswers(ctx, e)
	if err != nil {
		return false, err
	}
	if !ready {
		// Somebody is still to answer. The message is stored either way,
		// so nothing is lost by waiting for them.
		return false, nil
	}

	if b.resolutions != nil {
		return b.resumeSealedAsk(ctx, pending, AskReply{Replies: replies, Complete: true})
	}

	ask, err := b.store.ClaimPendingAsk(ctx, e.InReplyTo)
	switch {
	case errors.Is(err, ErrAskNotFound), errors.Is(err, ErrAskAlreadyClaimed):
		return false, nil
	case err != nil:
		return false, err
	}
	if !sameAskBinding(pending, ask) {
		return false, ErrReplyBinding
	}
	if b.resumer == nil {
		return false, nil
	}

	payload, err := json.Marshal(AskReply{Replies: replies, Complete: true})
	if err != nil {
		return false, err
	}
	if err := b.resumer.ResumeAgentReply(ctx, ask.AskerRunID, ask.ToolCallID, string(payload)); err != nil {
		return false, err
	}
	return true, nil
}

func askReplyItem(e *Envelope) AskReplyItem {
	return AskReplyItem{
		Performative:   string(e.Performative),
		Sender:         e.Sender.String(),
		Content:        e.Content,
		ConversationID: e.ConversationID.String(),
	}
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
	if err := b.checkRosterAddress(ctx, Address{Agent: agentName}); err != nil {
		return nil, err
	}
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
		if rosterErr := b.checkRosterEnvelope(ctx, e); rosterErr != nil {
			return nil, rosterErr
		}
		if b.roster != nil && (d.Scope.Canonical() != b.roster.scope || d.Receiver != (Address{Agent: agentName}) || !slices.Contains(e.Receivers, d.Receiver)) {
			return nil, ErrRosterDenied
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
	}
	for _, d := range rows {
		if err := b.store.MarkDeliveryRead(ctx, d.ID); err != nil {
			return nil, err
		}
	}
	return items, nil
}
