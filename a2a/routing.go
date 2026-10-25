package a2a

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// ErrRoutingUnavailable means the store cannot isolate orchestration traffic.
var ErrRoutingUnavailable = errors.New("cortex: a2a: orchestration routing unavailable")

// RoutingBackend provides persistence that partitions every A2A operation.
type RoutingBackend interface {
	Store
	SupportsOrchestrationRouting() bool
}

type routingKey struct{}

type routingDiscoveryKey struct{}

// ScopedRoutingBackend also restricts queue discovery, reclaim and ask expiry
// when RoutingDiscoveryScope is present. Roster-bound buses require it.
type ScopedRoutingBackend interface {
	RoutingBackend
	SupportsScopedOrchestrationDiscovery() bool
}

// RoutingDiscoveryScope returns the exact scope selected for background work.
// Without a binding, existing global queue discovery retains its scope policy.
func RoutingDiscoveryScope(ctx context.Context) (cortex.Scope, bool, error) {
	bound, ok := ctx.Value(routingDiscoveryKey{}).(bool)
	if !ok || !bound {
		return cortex.Scope{}, false, nil
	}
	scope, err := cortex.RequireScope(ctx)
	return scope, true, err
}

func newBusRoutingStore(store Store, team id.OrchestrationID, roster *frozenRoster) (Store, error) {
	wrapped, err := NewRoutingStore(store, team)
	if err != nil || roster == nil {
		return wrapped, err
	}
	bound, ok := wrapped.(*routingStore)
	if !ok {
		return nil, ErrRoutingUnavailable
	}
	backend, ok := bound.inner.(ScopedRoutingBackend)
	if !ok || !backend.SupportsScopedOrchestrationDiscovery() {
		return nil, ErrRoutingUnavailable
	}
	scope := roster.discoveryScope
	bound.discoveryScope = &scope
	return bound, nil
}

// RoutingOrchestration is the partition selected by NewRoutingStore.
// Backends must apply it to every read, mutation and dispatcher query.
// Empty selects legacy global traffic, never all partitions.
func RoutingOrchestration(ctx context.Context) string {
	key, ok := ctx.Value(routingKey{}).(string)
	if !ok {
		return ""
	}
	return key
}

// NewRoutingStore binds operations to one orchestration independently of scope.
// Zero selects global traffic. A nonzero ID requires a qualified backend.
// Binding does not authorize participants or provision an execution environment.
func NewRoutingStore(s Store, key id.OrchestrationID) (Store, error) {
	if s == nil {
		return nil, ErrNoStore
	}
	if bound, ok := s.(*routingStore); ok {
		s = bound.inner
	}
	if !key.IsNil() {
		backend, ok := s.(RoutingBackend)
		if key.Prefix() != id.PrefixOrchestration || !ok || !backend.SupportsOrchestrationRouting() {
			return nil, ErrRoutingUnavailable
		}
	}
	return &routingStore{inner: s, orchestration: key.String()}, nil
}

type routingStore struct {
	discoveryScope *cortex.Scope
	inner          Store
	orchestration  string
}

func (s *routingStore) discoveryContext(ctx context.Context) context.Context {
	ctx = s.context(ctx)
	if s.discoveryScope == nil {
		return ctx
	}
	return context.WithValue(cortex.WithScope(ctx, *s.discoveryScope), routingDiscoveryKey{}, true)
}

var _ Store = (*routingStore)(nil)

func (s *routingStore) context(ctx context.Context) context.Context {
	return context.WithValue(ctx, routingKey{}, s.orchestration)
}

func (s *routingStore) CreateMessage(ctx context.Context, e *Envelope) error {
	return s.inner.CreateMessage(s.context(ctx), e)
}

func (s *routingStore) GetMessage(ctx context.Context, msgID id.MessageID) (*Envelope, error) {
	return s.inner.GetMessage(s.context(ctx), msgID)
}

func (s *routingStore) ListMessages(ctx context.Context, filter *MessageListFilter) ([]*Envelope, error) {
	return s.inner.ListMessages(s.context(ctx), filter)
}

func (s *routingStore) CreateConversation(ctx context.Context, c *Conversation) error {
	return s.inner.CreateConversation(s.context(ctx), c)
}

func (s *routingStore) GetConversation(ctx context.Context, convID id.ConversationID) (*Conversation, error) {
	return s.inner.GetConversation(s.context(ctx), convID)
}

func (s *routingStore) UpdateConversation(ctx context.Context, c *Conversation) error {
	return s.inner.UpdateConversation(s.context(ctx), c)
}

func (s *routingStore) GetConversationByPeerContext(ctx context.Context, node, peerContext string) (*Conversation, error) {
	return s.inner.GetConversationByPeerContext(s.context(ctx), node, peerContext)
}

func (s *routingStore) ListConversations(ctx context.Context, filter *ConversationListFilter) ([]*Conversation, error) {
	return s.inner.ListConversations(s.context(ctx), filter)
}

func (s *routingStore) CreateDelivery(ctx context.Context, d *Delivery) error {
	return s.inner.CreateDelivery(s.context(ctx), d)
}

func (s *routingStore) GetDelivery(ctx context.Context, deliveryID id.DeliveryID) (*Delivery, error) {
	return s.inner.GetDelivery(s.context(ctx), deliveryID)
}

func (s *routingStore) UpdateDelivery(ctx context.Context, d *Delivery) error {
	return s.inner.UpdateDelivery(s.context(ctx), d)
}

func (s *routingStore) ClaimDelivery(ctx context.Context, deliveryID id.DeliveryID) (*Delivery, error) {
	return s.inner.ClaimDelivery(s.context(ctx), deliveryID)
}

func (s *routingStore) ListInbox(ctx context.Context, agentName string, filter InboxFilter) ([]*Delivery, error) {
	return s.inner.ListInbox(s.context(ctx), agentName, filter)
}

func (s *routingStore) ListQueuedDeliveries(ctx context.Context, limit int) ([]*Delivery, error) {
	return s.inner.ListQueuedDeliveries(s.discoveryContext(ctx), limit)
}

func (s *routingStore) MarkDeliveryRead(ctx context.Context, deliveryID id.DeliveryID) error {
	return s.inner.MarkDeliveryRead(s.context(ctx), deliveryID)
}

func (s *routingStore) ReclaimStaleDeliveries(ctx context.Context, olderThan time.Time, limit int) (int, error) {
	return s.inner.ReclaimStaleDeliveries(s.discoveryContext(ctx), olderThan, limit)
}

func (s *routingStore) CreatePendingAsk(ctx context.Context, a *PendingAsk) error {
	return s.inner.CreatePendingAsk(s.context(ctx), a)
}

func (s *routingStore) ClaimPendingAsk(ctx context.Context, replyWith string) (*PendingAsk, error) {
	return s.inner.ClaimPendingAsk(s.context(ctx), replyWith)
}

func (s *routingStore) ListExpiredAsks(ctx context.Context, now time.Time, limit int) ([]*PendingAsk, error) {
	return s.inner.ListExpiredAsks(s.discoveryContext(ctx), now, limit)
}

func (s *routingStore) ListPendingAsksByConversation(ctx context.Context, convID id.ConversationID) ([]*PendingAsk, error) {
	return s.inner.ListPendingAsksByConversation(s.context(ctx), convID)
}
