package a2a

import (
	"errors"
	"sync"
	"time"

	"context"

	"github.com/xraph/cortex/id"
)

func errorsIs(err, target error) bool { return errors.Is(err, target) }

// fakeClock is the package's test time source. Nothing in a2a reads the
// wall clock directly, so a deadline test moves this instead of sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// memStore is an in-memory Store. Insertion order is kept because tests
// assert on it: a conversation reads as a transcript, not as a set.
type memStore struct {
	mu sync.Mutex

	messages    map[string]*Envelope
	messageIDs  []string
	convs       map[string]*Conversation
	convIDs     []string
	deliveries  map[string]*Delivery
	deliveryIDs []string
	asks        map[string]*PendingAsk
	askKeys     []string

	// claimErrs are returned by the next N ClaimDelivery calls, standing
	// in for a store that is momentarily busy.
	claimErrs int
}

func (s *memStore) failNextClaims(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimErrs = n
}

func newMemStore() *memStore {
	return &memStore{
		messages:   map[string]*Envelope{},
		convs:      map[string]*Conversation{},
		deliveries: map[string]*Delivery{},
		asks:       map[string]*PendingAsk{},
	}
}

func (s *memStore) CreateMessage(_ context.Context, e *Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *e
	s.messages[e.ID.String()] = &cp
	s.messageIDs = append(s.messageIDs, e.ID.String())
	return nil
}

func (s *memStore) GetMessage(_ context.Context, msgID id.MessageID) (*Envelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.messages[msgID.String()]
	if !ok {
		return nil, errors.New("cortex: a2a: message not found")
	}
	cp := *e
	return &cp, nil
}

func (s *memStore) ListMessages(_ context.Context, f *MessageListFilter) ([]*Envelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Envelope
	for _, key := range s.messageIDs {
		e := s.messages[key]
		if f != nil && !f.ConversationID.IsNil() && e.ConversationID != f.ConversationID {
			continue
		}
		cp := *e
		out = append(out, &cp)
	}
	return out, nil
}

func (s *memStore) CreateConversation(_ context.Context, c *Conversation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *c
	s.convs[c.ID.String()] = &cp
	s.convIDs = append(s.convIDs, c.ID.String())
	return nil
}

func (s *memStore) GetConversation(_ context.Context, convID id.ConversationID) (*Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.convs[convID.String()]
	if !ok {
		return nil, errors.New("cortex: a2a: conversation not found")
	}
	cp := *c
	return &cp, nil
}

func (s *memStore) UpdateConversation(_ context.Context, c *Conversation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.convs[c.ID.String()]; !ok {
		return errors.New("cortex: a2a: conversation not found")
	}
	cp := *c
	s.convs[c.ID.String()] = &cp
	return nil
}

func (s *memStore) ListConversations(_ context.Context, f *ConversationListFilter) ([]*Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Conversation
	for _, key := range s.convIDs {
		c := s.convs[key]
		if f != nil && f.Status != "" && c.Status != f.Status {
			continue
		}
		cp := *c
		out = append(out, &cp)
	}
	return out, nil
}

func (s *memStore) CreateDelivery(_ context.Context, d *Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *d
	s.deliveries[d.ID.String()] = &cp
	s.deliveryIDs = append(s.deliveryIDs, d.ID.String())
	return nil
}

func (s *memStore) GetDelivery(_ context.Context, deliveryID id.DeliveryID) (*Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.deliveries[deliveryID.String()]
	if !ok {
		return nil, ErrDeliveryNotFound
	}
	cp := *d
	return &cp, nil
}

func (s *memStore) UpdateDelivery(_ context.Context, d *Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.deliveries[d.ID.String()]; !ok {
		return ErrDeliveryNotFound
	}
	cp := *d
	s.deliveries[d.ID.String()] = &cp
	return nil
}

func (s *memStore) ClaimDelivery(_ context.Context, deliveryID id.DeliveryID) (*Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimErrs > 0 {
		s.claimErrs--
		return nil, errors.New("database is locked")
	}
	d, ok := s.deliveries[deliveryID.String()]
	if !ok {
		return nil, ErrDeliveryNotFound
	}
	if d.State != DeliveryQueued {
		return nil, ErrDeliveryAlreadyClaimed
	}
	d.State = DeliveryDelivering
	cp := *d
	return &cp, nil
}

func (s *memStore) ListInbox(_ context.Context, agentName string, f InboxFilter) ([]*Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Delivery
	for _, key := range s.deliveryIDs {
		d := s.deliveries[key]
		if d.Receiver.Agent != agentName || d.State != DeliveryDelivered {
			continue
		}
		if f.UnreadOnly && d.ReadAt != nil {
			continue
		}
		if !f.ConversationID.IsNil() {
			e, ok := s.messages[d.MessageID.String()]
			if !ok || e.ConversationID != f.ConversationID {
				continue
			}
		}
		cp := *d
		out = append(out, &cp)
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out, nil
}

func (s *memStore) ListQueuedDeliveries(_ context.Context, limit int) ([]*Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Delivery
	for _, key := range s.deliveryIDs {
		d := s.deliveries[key]
		if d.State != DeliveryQueued {
			continue
		}
		cp := *d
		out = append(out, &cp)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *memStore) MarkDeliveryRead(_ context.Context, deliveryID id.DeliveryID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.deliveries[deliveryID.String()]
	if !ok {
		return ErrDeliveryNotFound
	}
	now := time.Now().UTC()
	d.ReadAt = &now
	return nil
}

func (s *memStore) CreatePendingAsk(_ context.Context, a *PendingAsk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *a
	s.asks[a.ReplyWith] = &cp
	s.askKeys = append(s.askKeys, a.ReplyWith)
	return nil
}

// ClaimPendingAsk is the whole reason this double exists: the claim happens
// under the lock, so a concurrent second claimant loses.
func (s *memStore) ClaimPendingAsk(_ context.Context, replyWith string) (*PendingAsk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.asks[replyWith]
	if !ok {
		return nil, ErrAskNotFound
	}
	if a.ClaimedAt != nil {
		return nil, ErrAskAlreadyClaimed
	}
	now := time.Now().UTC()
	a.ClaimedAt = &now
	cp := *a
	return &cp, nil
}

func (s *memStore) ListExpiredAsks(_ context.Context, now time.Time, limit int) ([]*PendingAsk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*PendingAsk
	for _, key := range s.askKeys {
		a := s.asks[key]
		if a.ClaimedAt != nil || a.Deadline == nil || a.Deadline.After(now) {
			continue
		}
		cp := *a
		out = append(out, &cp)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *memStore) ListPendingAsksByConversation(_ context.Context, convID id.ConversationID) ([]*PendingAsk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*PendingAsk
	for _, key := range s.askKeys {
		a := s.asks[key]
		if a.ClaimedAt != nil || a.ConversationID != convID {
			continue
		}
		cp := *a
		out = append(out, &cp)
	}
	return out, nil
}

var _ Store = (*memStore)(nil)
