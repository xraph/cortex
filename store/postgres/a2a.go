package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
)

// mutableA2AConversationColumns is every cortex_a2a_conversations column
// UpdateConversation may write. The five scope columns are deliberately
// absent: a conversation's scope is set once at creation, and grove builds
// SET from every model field by default, so without this whitelist an
// update issued from a broader (but still matching) context would widen
// the row's stored scope.
var mutableA2AConversationColumns = []string{
	"protocol",
	"peer_node",
	"peer_context",
	"participants",
	"status",
	"hop_ceiling",
	"hops_used",
	"deadline",
	"updated_at",
}

// mutableA2ADeliveryColumns mirrors the above for cortex_a2a_deliveries.
// message_id and receiver are absent as well as scope: a delivery never
// changes who it is for, only how far along it is.
var mutableA2ADeliveryColumns = []string{
	"state",
	"error",
	"claimed_at",
	"delivered_at",
	"read_at",
	"run_id",
	"updated_at",
}

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
	now := time.Now().UTC()
	e.CreatedAt = now
	e.UpdatedAt = now
	e.Scope = scope
	if _, err := s.pgdb.NewInsert(a2aMessageToModel(e)).Exec(ctx); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex: create a2a message: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex: create a2a message: %w", err)
	}
	return nil
}

func (s *Store) GetMessage(ctx context.Context, msgID id.MessageID) (*a2a.Envelope, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(a2aMessageModel)
	q := s.pgdb.NewSelect(m).Where("id = ?", msgID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err := q.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, a2a.ErrMessageNotFound
		}
		return nil, fmt.Errorf("cortex: get a2a message: %w", err)
	}
	return a2aMessageFromModel(m)
}

// ListMessages returns a conversation's messages oldest first, because a
// conversation is read as a transcript.
func (s *Store) ListMessages(ctx context.Context, filter *a2a.MessageListFilter) ([]*a2a.Envelope, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var models []a2aMessageModel
	q := s.pgdb.NewSelect(&models).OrderExpr("created_at ASC, id ASC")
	exact := filter != nil && filter.Exact
	for _, p := range scopePredicates(scope, exact) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if filter != nil {
		if !filter.ConversationID.IsNil() {
			q = q.Where("conversation_id = ?", filter.ConversationID.String())
		}
		if filter.Limit > 0 {
			q = q.Limit(filter.Limit)
		}
		if filter.Offset > 0 {
			q = q.Offset(filter.Offset)
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list a2a messages: %w", err)
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
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now
	c.Scope = scope
	if _, err := s.pgdb.NewInsert(a2aConversationToModel(c)).Exec(ctx); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex: create a2a conversation: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex: create a2a conversation: %w", err)
	}
	return nil
}

func (s *Store) GetConversation(ctx context.Context, convID id.ConversationID) (*a2a.Conversation, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	m := new(a2aConversationModel)
	q := s.pgdb.NewSelect(m).Where("id = ?", convID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err := q.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, a2a.ErrConversationNotFound
		}
		return nil, fmt.Errorf("cortex: get a2a conversation: %w", err)
	}
	return a2aConversationFromModel(m)
}

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

	m := new(a2aConversationModel)
	q := s.pgdb.NewSelect(m).
		Where("peer_node = ?", node).
		Where("peer_context = ?", peerContext)
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err := q.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, a2a.ErrConversationNotFound
		}
		return nil, fmt.Errorf("cortex: get a2a conversation by peer context: %w", err)
	}
	return a2aConversationFromModel(m)
}

func (s *Store) UpdateConversation(ctx context.Context, c *a2a.Conversation) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	c.UpdatedAt = time.Now().UTC()
	q := s.pgdb.NewUpdate(a2aConversationToModel(c)).
		Column(mutableA2AConversationColumns...).
		Where("id = ?", c.ID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: update a2a conversation: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex: update a2a conversation rows affected: %w", rowsErr)
	}
	if n == 0 {
		return a2a.ErrConversationNotFound
	}
	return nil
}

func (s *Store) ListConversations(ctx context.Context, filter *a2a.ConversationListFilter) ([]*a2a.Conversation, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var models []a2aConversationModel
	q := s.pgdb.NewSelect(&models).OrderExpr("created_at DESC, id DESC")
	exact := filter != nil && filter.Exact
	for _, p := range scopePredicates(scope, exact) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if filter != nil {
		if filter.Status != "" {
			q = q.Where("status = ?", filter.Status)
		}
		if filter.Limit > 0 {
			q = q.Limit(filter.Limit)
		}
		if filter.Offset > 0 {
			q = q.Offset(filter.Offset)
		}
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list a2a conversations: %w", err)
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
	now := time.Now().UTC()
	d.CreatedAt = now
	d.UpdatedAt = now
	d.Scope = scope
	if _, err := s.pgdb.NewInsert(a2aDeliveryToModel(d)).Exec(ctx); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex: create a2a delivery: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex: create a2a delivery: %w", err)
	}
	return nil
}

// GetDelivery reads one delivery row within the caller's scope.
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
	d.UpdatedAt = time.Now().UTC()
	q := s.pgdb.NewUpdate(a2aDeliveryToModel(d)).
		Column(mutableA2ADeliveryColumns...).
		Where("id = ?", d.ID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: update a2a delivery: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex: update a2a delivery rows affected: %w", rowsErr)
	}
	if n == 0 {
		return a2a.ErrDeliveryNotFound
	}
	return nil
}

// ClaimDelivery takes a queued delivery and marks it delivering. The
// state = 'queued' predicate is the claim: two workers racing both issue
// this UPDATE and exactly one of them changes a row, which is what stops
// one directive starting two runs.
func (s *Store) ClaimDelivery(ctx context.Context, deliveryID id.DeliveryID) (*a2a.Delivery, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	now := time.Now().UTC()
	q := s.pgdb.NewUpdate((*a2aDeliveryModel)(nil)).
		Set("state = ?", a2a.DeliveryDelivering).
		Set("claimed_at = ?", now).
		Set("updated_at = ?", now).
		Where("id = ?", deliveryID.String()).
		Where("state = ?", a2a.DeliveryQueued)
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("cortex: claim a2a delivery: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return nil, fmt.Errorf("cortex: claim a2a delivery rows affected: %w", rowsErr)
	}
	if n == 0 {
		// Nothing changed for one of two reasons and the caller has to
		// tell them apart: losing a race is ordinary, and a delivery id
		// nobody minted is a bug further up.
		exists, existsErr := s.deliveryExists(ctx, scope, deliveryID)
		if existsErr != nil {
			return nil, existsErr
		}
		if exists {
			return nil, a2a.ErrDeliveryAlreadyClaimed
		}
		return nil, a2a.ErrDeliveryNotFound
	}
	return s.getDelivery(ctx, scope, deliveryID)
}

// ListInbox returns messages that have ARRIVED for an agent. A queued
// delivery is deliberately excluded: it has not reached anyone yet, and an
// inbox that showed it would be showing mail that is still in transit.
func (s *Store) ListInbox(ctx context.Context, agentName string, filter a2a.InboxFilter) ([]*a2a.Delivery, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var models []a2aDeliveryModel
	q := s.pgdb.NewSelect(&models).
		Where("receiver_agent = ?", agentName).
		Where("state = ?", a2a.DeliveryDelivered).
		OrderExpr("created_at ASC, id ASC")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if filter.UnreadOnly {
		q = q.Where("read_at IS NULL")
	}
	if !filter.ConversationID.IsNil() {
		q = q.Where("message_id IN (SELECT id FROM cortex_a2a_messages WHERE conversation_id = ?)", filter.ConversationID.String())
	}
	if filter.Limit > 0 {
		q = q.Limit(filter.Limit)
	}
	if filter.Offset > 0 {
		q = q.Offset(filter.Offset)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list a2a inbox: %w", err)
	}
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

// ListQueuedDeliveries deliberately does NOT filter by scope.
//
// It is the dispatcher's read, and the dispatcher runs per process rather
// than per tenant: a delivery queued under one scope has to be carried
// even when nobody from that scope is currently calling in. Every row it
// returns carries its own scope, and deliverOne puts that scope back on
// the context before touching anything, so the isolation the rest of this
// file enforces is preserved by the caller instead of by the query. This
// mirrors the sweeper's cross-scope read of expired suspensions.
func (s *Store) ListQueuedDeliveries(ctx context.Context, limit int) ([]*a2a.Delivery, error) {
	var models []a2aDeliveryModel
	q := s.pgdb.NewSelect(&models).
		Where("state = ?", a2a.DeliveryQueued).
		OrderExpr("created_at ASC, id ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list queued a2a deliveries: %w", err)
	}
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

// ReclaimStaleDeliveries puts abandoned deliveries back in the queue.
//
// Like ListQueuedDeliveries it crosses scopes deliberately: the
// dispatcher runs per process rather than per tenant, and a row stranded
// by a dead worker belongs to whoever is alive now. The claimed_at
// predicate is what keeps a delivery somebody is still carrying out of
// it, which is the difference between recovering a message and
// delivering it twice.
func (s *Store) ReclaimStaleDeliveries(ctx context.Context, olderThan time.Time, limit int) (int, error) {
	// The rows are selected first and updated by id, mirroring the
	// sqlite implementation. The claim predicate stays on the update, so
	// a worker that comes back to life still wins its own row.
	var models []a2aDeliveryModel
	q := s.pgdb.NewSelect(&models).
		Where("state = ?", a2a.DeliveryDelivering).
		Where("claimed_at IS NOT NULL").
		Where("claimed_at < ?", olderThan.UTC()).
		OrderExpr("claimed_at ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Scan(ctx); err != nil {
		return 0, fmt.Errorf("cortex: list stale a2a deliveries: %w", err)
	}

	var n int
	now := time.Now().UTC()
	for i := range models {
		res, err := s.pgdb.NewUpdate((*a2aDeliveryModel)(nil)).
			Set("state = ?", a2a.DeliveryQueued).
			Set("claimed_at = ?", nil).
			Set("updated_at = ?", now).
			Where("id = ?", models[i].ID).
			Where("state = ?", a2a.DeliveryDelivering).
			Exec(ctx)
		if err != nil {
			return n, fmt.Errorf("cortex: reclaim a2a delivery: %w", err)
		}
		affected, rowsErr := res.RowsAffected()
		if rowsErr != nil {
			return n, fmt.Errorf("cortex: reclaim a2a delivery rows affected: %w", rowsErr)
		}
		n += int(affected)
	}
	return n, nil
}

func (s *Store) MarkDeliveryRead(ctx context.Context, deliveryID id.DeliveryID) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	now := time.Now().UTC()
	q := s.pgdb.NewUpdate((*a2aDeliveryModel)(nil)).
		Set("read_at = ?", now).
		Set("updated_at = ?", now).
		Where("id = ?", deliveryID.String()).
		Where("read_at IS NULL")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return fmt.Errorf("cortex: mark a2a delivery read: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("cortex: mark a2a delivery read rows affected: %w", rowsErr)
	}
	if n == 0 {
		// Already read is not an error. Two readers draining one inbox is
		// ordinary, and the second one has nothing to report.
		exists, existsErr := s.deliveryExists(ctx, scope, deliveryID)
		if existsErr != nil {
			return existsErr
		}
		if !exists {
			return a2a.ErrDeliveryNotFound
		}
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
	now := time.Now().UTC()
	a.CreatedAt = now
	a.UpdatedAt = now
	a.Scope = scope
	if _, err := s.pgdb.NewInsert(a2aPendingAskToModel(a)).Exec(ctx); err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("cortex: create a2a pending ask: %w", cortex.ErrAlreadyExists)
		}
		return fmt.Errorf("cortex: create a2a pending ask: %w", err)
	}
	return nil
}

// ClaimPendingAsk takes the ask carrying replyWith, but only if nobody has
// yet. The claimed_at IS NULL predicate is the whole guarantee: a late
// reply, the deadline sweep and a cancel can all reach for one row, and
// exactly one of them changes it and goes on to resume the waiting run.
func (s *Store) ClaimPendingAsk(ctx context.Context, replyWith string) (*a2a.PendingAsk, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	now := time.Now().UTC()
	q := s.pgdb.NewUpdate((*a2aPendingAskModel)(nil)).
		Set("claimed_at = ?", now).
		Set("updated_at = ?", now).
		Where("reply_with = ?", replyWith).
		Where("claimed_at IS NULL")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("cortex: claim a2a pending ask: %w", err)
	}
	n, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return nil, fmt.Errorf("cortex: claim a2a pending ask rows affected: %w", rowsErr)
	}
	if n == 0 {
		exists, existsErr := s.pendingAskExists(ctx, scope, replyWith)
		if existsErr != nil {
			return nil, existsErr
		}
		if exists {
			return nil, a2a.ErrAskAlreadyClaimed
		}
		return nil, a2a.ErrAskNotFound
	}
	return s.getPendingAsk(ctx, scope, replyWith)
}

// ListExpiredAsks returns unclaimed asks past their deadline. Claimed rows
// are excluded, which is what makes sweeping the same backlog twice safe.
func (s *Store) ListExpiredAsks(ctx context.Context, now time.Time, limit int) ([]*a2a.PendingAsk, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var models []a2aPendingAskModel
	q := s.pgdb.NewSelect(&models).
		Where("claimed_at IS NULL").
		Where("deadline IS NOT NULL").
		Where("deadline <= ?", now.UTC()).
		OrderExpr("deadline ASC")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list expired a2a asks: %w", err)
	}
	return a2aPendingAsksFromModels(models)
}

func (s *Store) ListPendingAsksByConversation(ctx context.Context, convID id.ConversationID) ([]*a2a.PendingAsk, error) {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, cortex.ErrNoScope
	}
	var models []a2aPendingAskModel
	q := s.pgdb.NewSelect(&models).
		Where("conversation_id = ?", convID.String()).
		Where("claimed_at IS NULL").
		OrderExpr("created_at ASC")
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("cortex: list a2a asks by conversation: %w", err)
	}
	return a2aPendingAsksFromModels(models)
}

// ──────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────

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
	m := new(a2aDeliveryModel)
	q := s.pgdb.NewSelect(m).Where("id = ?", deliveryID.String())
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err := q.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, a2a.ErrDeliveryNotFound
		}
		return nil, fmt.Errorf("cortex: get a2a delivery: %w", err)
	}
	return a2aDeliveryFromModel(m)
}

func (s *Store) deliveryExists(ctx context.Context, scope cortex.Scope, deliveryID id.DeliveryID) (bool, error) {
	_, err := s.getDelivery(ctx, scope, deliveryID)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, a2a.ErrDeliveryNotFound):
		return false, nil
	default:
		return false, err
	}
}

func (s *Store) getPendingAsk(ctx context.Context, scope cortex.Scope, replyWith string) (*a2a.PendingAsk, error) {
	m := new(a2aPendingAskModel)
	q := s.pgdb.NewSelect(m).Where("reply_with = ?", replyWith)
	for _, p := range scopePredicates(scope, false) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err := q.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, a2a.ErrAskNotFound
		}
		return nil, fmt.Errorf("cortex: get a2a pending ask: %w", err)
	}
	return a2aPendingAskFromModel(m)
}

func (s *Store) pendingAskExists(ctx context.Context, scope cortex.Scope, replyWith string) (bool, error) {
	_, err := s.getPendingAsk(ctx, scope, replyWith)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, a2a.ErrAskNotFound):
		return false, nil
	default:
		return false, err
	}
}
