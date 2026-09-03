package mongo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
)

// ──────────────────────────────────────────────────
// Messages
// ──────────────────────────────────────────────────

// CreateMessage persists an envelope, stamping the scope from the context.
// Envelopes are immutable once written, so there is no update counterpart.
func (s *Store) CreateMessage(ctx context.Context, e *a2a.Envelope) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	t := now()
	e.CreatedAt = t
	e.UpdatedAt = t
	e.Scope = scope

	if _, err := s.mdb.NewInsert(a2aMessageToModel(e)).Exec(ctx); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/mongo: create a2a message: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex/mongo: create a2a message: %w", err)
	}
	return nil
}

func (s *Store) GetMessage(ctx context.Context, msgID id.MessageID) (*a2a.Envelope, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var m a2aMessageModel

	filter := bson.M{"_id": msgID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}
	if err := s.mdb.NewFind(&m).Filter(filter).Scan(ctx); err != nil {
		if isNoDocuments(err) {
			return nil, a2a.ErrMessageNotFound
		}
		return nil, fmt.Errorf("cortex/mongo: get a2a message: %w", err)
	}
	return a2aMessageFromModel(&m)
}

// ListMessages returns a conversation's messages oldest first, because a
// conversation is read as a transcript.
func (s *Store) ListMessages(ctx context.Context, f *a2a.MessageListFilter) ([]*a2a.Envelope, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var models []a2aMessageModel

	exact := f != nil && f.Exact
	filter := bson.M{}
	for k, v := range scopeFilter(scope, exact) {
		filter[k] = v
	}
	if f != nil && !f.ConversationID.IsNil() {
		filter["conversation_id"] = f.ConversationID.String()
	}

	q := s.mdb.NewFind(&models).Filter(filter).Sort(bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}})
	if f != nil {
		if f.Limit > 0 {
			q = q.Limit(int64(f.Limit))
		}
		if f.Offset > 0 {
			q = q.Skip(int64(f.Offset))
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/mongo: list a2a messages: %w", err)
	}
	out := make([]*a2a.Envelope, len(models))
	for i := range models {
		e, convErr := a2aMessageFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		out[i] = e
	}
	return out, nil
}

// ──────────────────────────────────────────────────
// Conversations
// ──────────────────────────────────────────────────

func (s *Store) CreateConversation(ctx context.Context, c *a2a.Conversation) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	t := now()
	c.CreatedAt = t
	c.UpdatedAt = t
	c.Scope = scope

	if _, err := s.mdb.NewInsert(a2aConversationToModel(c)).Exec(ctx); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/mongo: create a2a conversation: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex/mongo: create a2a conversation: %w", err)
	}
	return nil
}

func (s *Store) GetConversation(ctx context.Context, convID id.ConversationID) (*a2a.Conversation, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var m a2aConversationModel

	filter := bson.M{"_id": convID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}
	if err := s.mdb.NewFind(&m).Filter(filter).Scan(ctx); err != nil {
		if isNoDocuments(err) {
			return nil, a2a.ErrConversationNotFound
		}
		return nil, fmt.Errorf("cortex/mongo: get a2a conversation: %w", err)
	}
	return a2aConversationFromModel(&m)
}

// UpdateConversation writes the mutable half of a conversation. The scope
// fields are deliberately absent from the $set: a conversation's scope is
// fixed at creation, and an update issued from a broader context would
// otherwise widen it.
// GetConversationByPeerContext finds the conversation opened for a
// remote thread. The node is half the key: two peers can use the same
// context id, and joining one peer's thread to another's would leak a
// conversation across a trust boundary.
func (s *Store) GetConversationByPeerContext(ctx context.Context, node, peerContext string) (*a2a.Conversation, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	if node == "" || peerContext == "" {
		// An empty pairing would match every locally started
		// conversation, which is the opposite of what the caller meant.
		return nil, a2a.ErrConversationNotFound
	}

	var m a2aConversationModel
	filter := bson.M{"peer_node": node, "peer_context": peerContext}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}
	if err := s.mdb.NewFind(&m).Filter(filter).Scan(ctx); err != nil {
		if isNoDocuments(err) {
			return nil, a2a.ErrConversationNotFound
		}
		return nil, fmt.Errorf("cortex/mongo: get a2a conversation by peer context: %w", err)
	}
	return a2aConversationFromModel(&m)
}

func (s *Store) UpdateConversation(ctx context.Context, c *a2a.Conversation) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	c.UpdatedAt = now()
	m := a2aConversationToModel(c)

	filter := bson.M{"_id": m.ID}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}
	set := bson.M{
		"protocol":     m.Protocol,
		"peer_node":    m.PeerNode,
		"peer_context": m.PeerContext,
		"participants": m.Participants,
		"status":       m.Status,
		"hop_ceiling":  m.HopCeiling,
		"hops_used":    m.HopsUsed,
		"deadline":     m.Deadline,
		"updated_at":   m.UpdatedAt,
	}

	res, err := s.mdb.NewUpdate((*a2aConversationModel)(nil)).
		Filter(filter).
		SetUpdate(bson.M{"$set": set}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update a2a conversation: %w", err)
	}
	if res.MatchedCount() == 0 {
		return a2a.ErrConversationNotFound
	}
	return nil
}

func (s *Store) ListConversations(ctx context.Context, f *a2a.ConversationListFilter) ([]*a2a.Conversation, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var models []a2aConversationModel

	exact := f != nil && f.Exact
	filter := bson.M{}
	for k, v := range scopeFilter(scope, exact) {
		filter[k] = v
	}
	if f != nil && f.Status != "" {
		filter["status"] = f.Status
	}

	q := s.mdb.NewFind(&models).Filter(filter).Sort(bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}})
	if f != nil {
		if f.Limit > 0 {
			q = q.Limit(int64(f.Limit))
		}
		if f.Offset > 0 {
			q = q.Skip(int64(f.Offset))
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/mongo: list a2a conversations: %w", err)
	}
	out := make([]*a2a.Conversation, len(models))
	for i := range models {
		c, convErr := a2aConversationFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		out[i] = c
	}
	return out, nil
}

// ──────────────────────────────────────────────────
// Deliveries
// ──────────────────────────────────────────────────

func (s *Store) CreateDelivery(ctx context.Context, d *a2a.Delivery) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	t := now()
	d.CreatedAt = t
	d.UpdatedAt = t
	d.Scope = scope

	if _, err := s.mdb.NewInsert(a2aDeliveryToModel(d)).Exec(ctx); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/mongo: create a2a delivery: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex/mongo: create a2a delivery: %w", err)
	}
	return nil
}

// GetDelivery reads one delivery document within the caller's scope.
func (s *Store) GetDelivery(ctx context.Context, deliveryID id.DeliveryID) (*a2a.Delivery, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	return s.getDelivery(ctx, scope, deliveryID)
}

func (s *Store) UpdateDelivery(ctx context.Context, d *a2a.Delivery) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	d.UpdatedAt = now()
	m := a2aDeliveryToModel(d)

	filter := bson.M{"_id": m.ID}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}
	set := bson.M{
		"state":        m.State,
		"error":        m.Error,
		"claimed_at":   m.ClaimedAt,
		"delivered_at": m.DeliveredAt,
		"read_at":      m.ReadAt,
		"run_id":       m.RunID,
		"updated_at":   m.UpdatedAt,
	}

	res, err := s.mdb.NewUpdate((*a2aDeliveryModel)(nil)).
		Filter(filter).
		SetUpdate(bson.M{"$set": set}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: update a2a delivery: %w", err)
	}
	if res.MatchedCount() == 0 {
		return a2a.ErrDeliveryNotFound
	}
	return nil
}

// ClaimDelivery takes a queued delivery and marks it delivering.
//
// FindOneAndUpdate applies the match and the write as one operation, so
// the state = "queued" filter is the claim: two workers racing both issue
// it and exactly one of them matches a document. That is what stops one
// directive starting two runs.
func (s *Store) ClaimDelivery(ctx context.Context, deliveryID id.DeliveryID) (*a2a.Delivery, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	t := now()
	filter := bson.M{"_id": deliveryID.String(), "state": a2a.DeliveryQueued}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}
	update := bson.M{"$set": bson.M{
		"state":      a2a.DeliveryDelivering,
		"claimed_at": t,
		"updated_at": t,
	}}

	res := s.mdb.Collection(colA2ADeliveries).FindOneAndUpdate(ctx, filter, update)
	if err := res.Err(); err != nil {
		if isNoDocuments(err) {
			// No match means one of two things and the caller has to tell
			// them apart: losing a race is ordinary, a delivery id nobody
			// minted is a bug further up.
			exists, existsErr := s.deliveryExists(ctx, scope, deliveryID)
			if existsErr != nil {
				return nil, existsErr
			}
			if exists {
				return nil, a2a.ErrDeliveryAlreadyClaimed
			}
			return nil, a2a.ErrDeliveryNotFound
		}
		return nil, fmt.Errorf("cortex/mongo: claim a2a delivery: %w", err)
	}
	return s.getDelivery(ctx, scope, deliveryID)
}

// ListInbox returns messages that have ARRIVED for an agent. A queued
// delivery is deliberately excluded: it has not reached anyone yet, and an
// inbox that showed it would be showing mail still in transit.
func (s *Store) ListInbox(ctx context.Context, agentName string, f a2a.InboxFilter) ([]*a2a.Delivery, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	filter := bson.M{"receiver_agent": agentName, "state": a2a.DeliveryDelivered}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}
	if f.UnreadOnly {
		filter["read_at"] = nil
	}
	if !f.ConversationID.IsNil() {
		// Mongo has no join, so the conversation's message ids are read
		// first and the delivery query filters on them.
		msgs, err := s.ListMessages(ctx, &a2a.MessageListFilter{ConversationID: f.ConversationID})
		if err != nil {
			return nil, err
		}
		ids := make([]string, len(msgs))
		for i, m := range msgs {
			ids[i] = m.ID.String()
		}
		filter["message_id"] = bson.M{"$in": ids}
	}

	var models []a2aDeliveryModel
	q := s.mdb.NewFind(&models).Filter(filter).Sort(bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}})
	if f.Limit > 0 {
		q = q.Limit(int64(f.Limit))
	}
	if f.Offset > 0 {
		q = q.Skip(int64(f.Offset))
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/mongo: list a2a inbox: %w", err)
	}
	return a2aDeliveriesFromModels(models)
}

// ListQueuedDeliveries deliberately does NOT filter by scope.
//
// It is the dispatcher's read, and the dispatcher runs per process rather
// than per tenant: a delivery queued under one scope has to be carried
// even when nobody from that scope is currently calling in. Every document
// it returns carries its own scope, and the caller puts that scope back on
// the context before touching anything, so isolation is preserved by the
// caller instead of by the query. This mirrors the sweeper's cross-scope
// read of expired suspensions.
func (s *Store) ListQueuedDeliveries(ctx context.Context, limit int) ([]*a2a.Delivery, error) {
	var models []a2aDeliveryModel
	q := s.mdb.NewFind(&models).
		Filter(bson.M{"state": a2a.DeliveryQueued}).
		Sort(bson.D{{Key: "created_at", Value: 1}, {Key: "_id", Value: 1}})
	if limit > 0 {
		q = q.Limit(int64(limit))
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/mongo: list queued a2a deliveries: %w", err)
	}
	return a2aDeliveriesFromModels(models)
}

// ReclaimStaleDeliveries puts abandoned deliveries back in the queue.
//
// Like the queued read it crosses scopes deliberately: the dispatcher
// runs per process rather than per tenant, and a document stranded by a
// dead worker belongs to whoever is alive now. The claimed_at filter is
// what keeps a delivery somebody is still carrying out of it, which is
// the difference between recovering a message and delivering it twice.
func (s *Store) ReclaimStaleDeliveries(ctx context.Context, olderThan time.Time, limit int) (int, error) {
	filter := bson.M{
		"state":      a2a.DeliveryDelivering,
		"claimed_at": bson.M{"$ne": nil, "$lt": olderThan.UTC()},
	}

	// UpdateMany has no limit, so the batch is bounded by reading the ids
	// first. The state filter stays on the update, so a worker that comes
	// back to life still wins its own document.
	var models []a2aDeliveryModel
	q := s.mdb.NewFind(&models).Filter(filter).Sort(bson.D{{Key: "claimed_at", Value: 1}})
	if limit > 0 {
		q = q.Limit(int64(limit))
	}
	if err := q.Scan(ctx); err != nil {
		return 0, fmt.Errorf("cortex/mongo: list stale a2a deliveries: %w", err)
	}

	var n int
	for i := range models {
		res, err := s.mdb.NewUpdate((*a2aDeliveryModel)(nil)).
			Filter(bson.M{"_id": models[i].ID, "state": a2a.DeliveryDelivering}).
			SetUpdate(bson.M{"$set": bson.M{
				"state":      a2a.DeliveryQueued,
				"claimed_at": nil,
				"updated_at": now(),
			}}).
			Exec(ctx)
		if err != nil {
			return n, fmt.Errorf("cortex/mongo: reclaim a2a delivery: %w", err)
		}
		n += int(res.ModifiedCount())
	}
	return n, nil
}

func (s *Store) MarkDeliveryRead(ctx context.Context, deliveryID id.DeliveryID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	t := now()
	filter := bson.M{"_id": deliveryID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	res, err := s.mdb.NewUpdate((*a2aDeliveryModel)(nil)).
		Filter(filter).
		SetUpdate(bson.M{"$set": bson.M{"read_at": t, "updated_at": t}}).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex/mongo: mark a2a delivery read: %w", err)
	}
	if res.MatchedCount() == 0 {
		return a2a.ErrDeliveryNotFound
	}
	return nil
}

// ──────────────────────────────────────────────────
// Pending asks
// ──────────────────────────────────────────────────

func (s *Store) CreatePendingAsk(ctx context.Context, a *a2a.PendingAsk) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	t := now()
	a.CreatedAt = t
	a.UpdatedAt = t
	a.Scope = scope

	if _, err := s.mdb.NewInsert(a2aPendingAskToModel(a)).Exec(ctx); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex/mongo: create a2a pending ask: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex/mongo: create a2a pending ask: %w", err)
	}
	return nil
}

// ClaimPendingAsk takes the ask carrying replyWith, but only if nobody
// has yet.
//
// The claimed_at: nil filter inside a FindOneAndUpdate is the whole
// guarantee: a late reply, the deadline sweep and a cancel can all reach
// for one document, and exactly one of them matches and goes on to resume
// the waiting run. The reply-with token is the document's _id, so there
// can only ever be one of them to race for.
func (s *Store) ClaimPendingAsk(ctx context.Context, replyWith string) (*a2a.PendingAsk, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	t := now()
	filter := bson.M{"_id": replyWith, "claimed_at": nil}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}
	update := bson.M{"$set": bson.M{"claimed_at": t, "updated_at": t}}

	res := s.mdb.Collection(colA2APendingAsks).FindOneAndUpdate(ctx, filter, update)
	if err := res.Err(); err != nil {
		if isNoDocuments(err) {
			exists, existsErr := s.pendingAskExists(ctx, scope, replyWith)
			if existsErr != nil {
				return nil, existsErr
			}
			if exists {
				return nil, a2a.ErrAskAlreadyClaimed
			}
			return nil, a2a.ErrAskNotFound
		}
		return nil, fmt.Errorf("cortex/mongo: claim a2a pending ask: %w", err)
	}
	return s.getPendingAsk(ctx, scope, replyWith)
}

// ListExpiredAsks returns unclaimed asks past their deadline. Claimed
// documents are excluded, which is what makes sweeping the same backlog
// twice safe.
func (s *Store) ListExpiredAsks(ctx context.Context, at time.Time, limit int) ([]*a2a.PendingAsk, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	filter := bson.M{
		"claimed_at": nil,
		"deadline":   bson.M{"$ne": nil, "$lte": at.UTC()},
	}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	var models []a2aPendingAskModel
	q := s.mdb.NewFind(&models).Filter(filter).Sort(bson.D{{Key: "deadline", Value: 1}})
	if limit > 0 {
		q = q.Limit(int64(limit))
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/mongo: list expired a2a asks: %w", err)
	}
	return a2aPendingAsksFromModels(models)
}

func (s *Store) ListPendingAsksByConversation(ctx context.Context, convID id.ConversationID) ([]*a2a.PendingAsk, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	filter := bson.M{"conversation_id": convID.String(), "claimed_at": nil}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}

	var models []a2aPendingAskModel
	if err := s.mdb.NewFind(&models).Filter(filter).Sort(bson.D{{Key: "created_at", Value: 1}}).Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex/mongo: list a2a asks by conversation: %w", err)
	}
	return a2aPendingAsksFromModels(models)
}

// ──────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────

func a2aDeliveriesFromModels(models []a2aDeliveryModel) ([]*a2a.Delivery, error) {
	out := make([]*a2a.Delivery, len(models))
	for i := range models {
		d, convErr := a2aDeliveryFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		out[i] = d
	}
	return out, nil
}

func a2aPendingAsksFromModels(models []a2aPendingAskModel) ([]*a2a.PendingAsk, error) {
	out := make([]*a2a.PendingAsk, len(models))
	for i := range models {
		a, convErr := a2aPendingAskFromModel(&models[i])
		if convErr != nil {
			return nil, convErr
		}
		out[i] = a
	}
	return out, nil
}

func (s *Store) getDelivery(ctx context.Context, scope cortex.Scope, deliveryID id.DeliveryID) (*a2a.Delivery, error) {
	var m a2aDeliveryModel
	filter := bson.M{"_id": deliveryID.String()}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}
	if err := s.mdb.NewFind(&m).Filter(filter).Scan(ctx); err != nil {
		if isNoDocuments(err) {
			return nil, a2a.ErrDeliveryNotFound
		}
		return nil, fmt.Errorf("cortex/mongo: get a2a delivery: %w", err)
	}
	return a2aDeliveryFromModel(&m)
}

func (s *Store) deliveryExists(ctx context.Context, scope cortex.Scope, deliveryID id.DeliveryID) (bool, error) {
	_, err := s.getDelivery(ctx, scope, deliveryID)
	switch {
	case err == nil:
		return true, nil
	case isNotFound(err, a2a.ErrDeliveryNotFound):
		return false, nil
	default:
		return false, err
	}
}

func (s *Store) getPendingAsk(ctx context.Context, scope cortex.Scope, replyWith string) (*a2a.PendingAsk, error) {
	var m a2aPendingAskModel
	filter := bson.M{"_id": replyWith}
	for k, v := range scopeFilter(scope, false) {
		filter[k] = v
	}
	if err := s.mdb.NewFind(&m).Filter(filter).Scan(ctx); err != nil {
		if isNoDocuments(err) {
			return nil, a2a.ErrAskNotFound
		}
		return nil, fmt.Errorf("cortex/mongo: get a2a pending ask: %w", err)
	}
	return a2aPendingAskFromModel(&m)
}

func (s *Store) pendingAskExists(ctx context.Context, scope cortex.Scope, replyWith string) (bool, error) {
	_, err := s.getPendingAsk(ctx, scope, replyWith)
	switch {
	case err == nil:
		return true, nil
	case isNotFound(err, a2a.ErrAskNotFound):
		return false, nil
	default:
		return false, err
	}
}
