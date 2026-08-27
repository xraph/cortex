package a2aremote

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

// defaultRemoteSenderName is who a peer is, as far as cortex is
// concerned, when it does not say which of its agents is speaking.
const defaultRemoteSenderName = "remote"

// defaultTaskPageSize caps a ListTasks page when the caller names none.
const defaultTaskPageSize = 50

// Options tunes the service.
type Options struct {
	// Card supplies the half of an agent card cortex cannot know about
	// itself, chiefly the URL peers reach it at.
	Card CardOptions
	// Exposed lists the agents whose cards are served. A card is public,
	// so exposure is opt-in: an empty list serves no cards at all.
	Exposed []string
	// Scope is where the exposed agents are read from.
	//
	// It has to be said explicitly because a card is fetched without
	// credentials: there is no caller to borrow a scope from, and every
	// store read in cortex needs one. Only agents named in Exposed are
	// ever read under it, so it grants no more reach than exposure
	// already did.
	Scope cortex.Scope
	// DefaultAgent also gets its card served at the root well-known
	// path, so plain discovery finds something.
	DefaultAgent string
	// Streaming turns on SendStreamingMessage and SubscribeToTask, and
	// makes the card advertise them. Off by default: a card that offers
	// a stream nobody serves is a promise the server cannot keep.
	Streaming bool
	// StreamPoll is how often a subscription re-reads its task. It
	// bounds how stale an update can be and nothing else.
	StreamPoll time.Duration
}

// Service holds every semantic decision the remote transport makes.
//
// The bindings translate formats and call in here. That is what lets
// three of them share one implementation and, more to the point, one set
// of security rules: a rule enforced here cannot be forgotten by the
// gRPC handler.
type Service struct {
	gw       Gateway
	resolver PeerResolver
	opts     Options
}

// NewService builds a Service, or returns nil when it cannot.
//
// A nil resolver returns nil rather than defaulting to something
// permissive. A service that authenticates nobody is an open door onto
// every agent in the process, and defaulting to it would be the kind of
// convenience that reads as a feature until it is a breach.
func NewService(gw Gateway, resolver PeerResolver, opts Options) *Service {
	if gw == nil || resolver == nil {
		return nil
	}
	return &Service{gw: gw, resolver: resolver, opts: opts}
}

// SendMessage carries an inbound message to the agent named by tenant.
func (s *Service) SendMessage(ctx context.Context, cred Credentials, req SendMessageRequest) (*SendMessageResult, error) {
	ctx, peer, err := s.authenticate(ctx, cred)
	if err != nil {
		return nil, err
	}
	if tenantErr := s.checkTenant(ctx, req.Tenant); tenantErr != nil {
		return nil, tenantErr
	}

	// The sender is built here, from the peer the credentials earned and
	// the name the peer claims, in that order of authority. The claimed
	// name only ever fills the agent half; the node half is ours.
	sender := a2a.Address{Agent: req.SenderName, Node: peer.Node}
	if sender.Agent == "" {
		sender.Agent = defaultRemoteSenderName
	}

	params, err := EnvelopeParamsFromMessage(req.Message, sender, a2a.Address{Agent: req.Tenant})
	if err != nil {
		return nil, err
	}

	sent, err := s.gw.SendMessage(ctx, params)
	if errors.Is(err, a2a.ErrConversationNotFound) {
		// The peer quoted a context id from its own world.
		//
		// A contextId names a conversation in whichever engine issued
		// it, and a peer continuing a thread on its side has no idea
		// whether that id means anything here. Rather than refuse a
		// perfectly good message, the exchange starts a conversation on
		// this side and the peer's id is kept as metadata, so a reader
		// can still line the two up later.
		params.ConversationID = id.ConversationID{}
		params.Metadata = withPeerContext(params.Metadata, req.Message.ContextID)
		sent, err = s.gw.SendMessage(ctx, params)
	}
	if err != nil {
		return nil, mapBusError(err)
	}

	// An informative was filed, so there is nothing to follow. A
	// directive is work, and the peer gets a task to poll and cancel.
	class, _ := params.Performative.Class()
	if class != a2a.ClassDirective {
		return &SendMessageResult{Message: &Message{
			MessageID: sent.MessageID.String(),
			ContextID: sent.ConversationID.String(),
			Role:      RoleAgent,
			Parts:     []Part{{Text: "received"}},
		}}, nil
	}

	// The task id is the DELIVERY id, not a run id.
	//
	// Nothing has run yet when this returns: the message is queued, and
	// a run only exists once a worker carries it. The delivery row is
	// the durable handle that exists right now, and GetTask follows it
	// to the run as soon as there is one. Naming a run here would mean
	// inventing an id for something that does not exist.
	task := Task{
		ID:        deliveryIDOf(sent),
		ContextID: sent.ConversationID.String(),
		Status:    TaskStatus{State: TaskStateSubmitted},
	}
	return &SendMessageResult{Task: &task}, nil
}

// GetTask projects a run as a task.
func (s *Service) GetTask(ctx context.Context, cred Credentials, req GetTaskRequest) (*Task, error) {
	ctx, _, err := s.authenticate(ctx, cred)
	if err != nil {
		return nil, err
	}
	r, err := s.loadRun(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	task := TaskFromRun(r, "")
	return &task, nil
}

// ListTasks projects this scope's runs as tasks.
func (s *Service) ListTasks(ctx context.Context, cred Credentials, req ListTasksRequest) (*ListTasksResult, error) {
	ctx, _, err := s.authenticate(ctx, cred)
	if err != nil {
		return nil, err
	}
	size := req.PageSize
	if size <= 0 {
		size = defaultTaskPageSize
	}
	runs, err := s.gw.ListRuns(ctx, &run.ListFilter{Limit: size})
	if err != nil {
		return nil, ErrInternal("could not list tasks")
	}
	out := make([]Task, 0, len(runs))
	for _, r := range runs {
		out = append(out, TaskFromRun(r, ""))
	}
	return &ListTasksResult{Tasks: out}, nil
}

// CancelTask stops a running task.
func (s *Service) CancelTask(ctx context.Context, cred Credentials, req CancelTaskRequest) (*Task, error) {
	ctx, _, err := s.authenticate(ctx, cred)
	if err != nil {
		return nil, err
	}
	r, err := s.loadRun(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	if taskState(r.State).Terminal() {
		return nil, ErrTaskNotCancelable(req.ID)
	}
	if err := s.gw.CancelRun(ctx, r.ID); err != nil {
		return nil, ErrInternal("could not cancel the task")
	}
	// Read the projection off the state we just moved it to rather than
	// re-reading: the store write has happened, and a second read would
	// only add a way for this to disagree with itself.
	task := TaskFromRun(&run.Run{ID: r.ID, State: run.StateCancelled}, "")
	return &task, nil
}

// authenticate resolves the caller and puts ITS scope on the context.
//
// Everything downstream reads the scope from the context, so this is the
// only place a scope enters an inbound request. No header, no message
// field and no query parameter reaches it.
func (s *Service) authenticate(ctx context.Context, cred Credentials) (context.Context, Peer, error) {
	peer, err := s.resolver.ResolvePeer(ctx, cred)
	if err != nil {
		// Deliberately opaque: a caller that has not proved who it is
		// learns that it was refused and nothing else about what is here.
		return ctx, Peer{}, ErrUnauthenticated()
	}
	if peer.Scope.IsZero() {
		// A resolver that answered with no scope did not really answer.
		// Letting it through would fail several layers down, at a store
		// call, with an error that says nothing about the cause.
		return ctx, Peer{}, ErrUnauthenticated()
	}
	if peer.Node == "" {
		// Without a node there is nothing to namespace the peer's sender
		// name under, and it would land looking like a local agent.
		return ctx, Peer{}, ErrUnauthenticated()
	}
	return cortex.WithScope(ctx, peer.Scope), peer, nil
}

// checkTenant refuses a tenant that names no agent here.
//
// The error is TaskNotFound rather than something about agents, so a
// caller cannot map out which agents exist by probing names.
func (s *Service) checkTenant(ctx context.Context, tenant string) error {
	if tenant == "" {
		return ErrInvalidParams("tenant is required: it names the agent this message is for")
	}
	if _, err := s.gw.GetAgentByName(ctx, tenant); err != nil {
		return ErrTaskNotFound(tenant)
	}
	return nil
}

// loadRun reads the run behind a task id.
//
// A task id is one of two things, because of when a peer got it. A
// delivery id is what SendMessage hands back, before any run exists. A
// run id is what a peer may have learned later. Both resolve here, and
// anything else is not found: a malformed id and a missing run get the
// same answer, for the same reason an unknown tenant does.
func (s *Service) loadRun(ctx context.Context, taskID string) (*run.Run, error) {
	parsed, err := id.Parse(taskID)
	if err != nil {
		return nil, ErrTaskNotFound(taskID)
	}

	switch parsed.Prefix() {
	case id.PrefixAgentRun:
		r, runErr := s.gw.GetRun(ctx, parsed)
		if runErr != nil {
			return nil, ErrTaskNotFound(taskID)
		}
		return r, nil

	case id.PrefixDelivery:
		d, delErr := s.gw.GetDelivery(ctx, parsed)
		if delErr != nil {
			return nil, ErrTaskNotFound(taskID)
		}
		if d.RunID.IsNil() {
			// Queued or carried, but nothing has started yet. That is a
			// real state rather than a missing task, so it gets a
			// synthetic run to project from.
			return &run.Run{ID: parsed, State: deliveryRunState(d)}, nil
		}
		r, runErr := s.gw.GetRun(ctx, d.RunID)
		if runErr != nil {
			return nil, ErrTaskNotFound(taskID)
		}
		return r, nil

	default:
		return nil, ErrTaskNotFound(taskID)
	}
}

// deliveryRunState reads a delivery that has not started a run as a run
// state, so one projection covers both.
func deliveryRunState(d *a2a.Delivery) run.State {
	switch d.State {
	case a2a.DeliveryFailed:
		return run.StateFailed
	case a2a.DeliveryQueued:
		return run.StateCreated
	default:
		// Delivering, or delivered to an inbox with no run behind it.
		return run.StateRunning
	}
}

// peerContextKey is where a peer's own conversation id is recorded when
// it does not name a conversation of ours.
const peerContextKey = "a2a.peer_context_id"

func withPeerContext(meta map[string]any, contextID string) map[string]any {
	if contextID == "" {
		return meta
	}
	if meta == nil {
		meta = map[string]any{}
	}
	meta[peerContextKey] = contextID
	return meta
}

// deliveryIDOf picks the handle a send produced. A directive addressed
// to one agent has exactly one delivery; the message id is the fallback
// for the case where the send produced none, which a caller can still
// quote back even though it will not resolve to work.
func deliveryIDOf(res *a2a.SendResult) string {
	for _, d := range res.Deliveries {
		if !d.DeliveryID.IsNil() {
			return d.DeliveryID.String()
		}
	}
	return res.MessageID.String()
}

// mapBusError turns a bus refusal into the protocol's own vocabulary.
// A hop ceiling and a closed conversation are the caller's problem to
// understand, so they keep their meaning; anything else is internal.
func mapBusError(err error) *Error {
	switch {
	case errors.Is(err, a2a.ErrHopCeiling):
		return ErrInvalidRequest("this conversation has used up its message budget")
	case errors.Is(err, a2a.ErrConversationClosed):
		return ErrInvalidRequest("this conversation is closed")
	case errors.Is(err, a2a.ErrSelfAddressed):
		return ErrInvalidParams("that message is addressed to its own sender")
	case errors.Is(err, a2a.ErrInvalidPerformative):
		return ErrInvalidParams("unknown performative")
	case errors.Is(err, a2a.ErrUnknownReceiver), errors.Is(err, a2a.ErrUnroutable):
		return ErrTaskNotFound("recipient")
	default:
		return ErrInternal("the message could not be delivered")
	}
}
