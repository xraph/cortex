package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

const kindHeldAskRelease = "a2a_held_release"

type heldAskTransaction interface {
	SuspensionTransaction
	MessagingTransaction
	LockPendingAsk(context.Context, string) (*a2a.PendingAsk, error)
}

// GetHeldAskRelease reads exact-scope queue identities, never dispatch authority.
func (s *Repository) GetHeldAskRelease(ctx context.Context, token string) (a2a.HeldAskReceipt, error) {
	var receipt a2a.HeldAskReceipt
	if !validAskToken(token) {
		return receipt, a2a.ErrHeldAskConflict
	}
	row, err := s.get(ctx, Query{Kind: kindHeldAskRelease, ID: resolutionKey(ctx, token)})
	if errors.Is(err, ErrAbsent) {
		return receipt, a2a.ErrHeldAskNotFound
	}
	if err != nil {
		return receipt, err
	}
	if row.Deleted || len(row.Body) > 64<<10 || row.Kind != kindHeldAskRelease || row.ID != resolutionKey(ctx, token) || row.RunID != resolutionPartition(ctx) || row.Digest != evidence.Digest([]byte(row.Body)) || json.Unmarshal([]byte(row.Body), &receipt) != nil || receipt.SchemaVersion != 1 || receipt.Scope.Canonical() != row.ScopeCanon || receipt.OrchestrationID != a2a.RoutingOrchestration(ctx) || receipt.Request.ReplyWith != token || !validHeldRequest(receipt.Request) || receipt.MessageID.Prefix() != id.PrefixMessage || len(receipt.Deliveries) == 0 || len(receipt.Deliveries) > 256 || receipt.CommittedAt.IsZero() {
		return a2a.HeldAskReceipt{}, a2a.ErrHeldAskConflict
	}
	seen := make(map[id.DeliveryID]bool, len(receipt.Deliveries))
	for _, delivery := range receipt.Deliveries {
		if delivery.Prefix() != id.PrefixDelivery || seen[delivery] {
			return a2a.HeldAskReceipt{}, a2a.ErrHeldAskConflict
		}
		seen[delivery] = true
	}
	return receipt, nil
}

func validHeldRequest(req a2a.HeldAskRequest) bool {
	return req.RunID.Prefix() == id.PrefixAgentRun && req.SuspensionID.Prefix() == id.PrefixSuspension && req.RunRevision >= 0 && validAskToken(req.ReplyWith)
}

// ReleaseHeldAsk commits one question's queue and receipt under its run lock.
func (s *Repository) ReleaseHeldAsk(ctx context.Context, req a2a.HeldAskRequest) (a2a.HeldAskReceipt, error) {
	var result a2a.HeldAskReceipt
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return result, err
	}
	if !validHeldRequest(req) {
		return result, a2a.ErrHeldAskConflict
	}
	driver, ok := s.driver.(RunDriver)
	if !ok {
		return result, a2a.ErrHeldAskUnavailable
	}
	err = driver.TransactRun(ctx, req.RunID, func(tc context.Context, base RunTransaction) error {
		result = a2a.HeldAskReceipt{}
		tx, supported := base.(heldAskTransaction)
		if !supported {
			return a2a.ErrHeldAskUnavailable
		}
		repo := s.inTransaction(base)
		prior, priorErr := repo.GetHeldAskRelease(tc, req.ReplyWith)
		if priorErr == nil {
			if prior.Request != req {
				return a2a.ErrHeldAskConflict
			}
			result = prior
			return tc.Err()
		}
		if !errors.Is(priorErr, a2a.ErrHeldAskNotFound) {
			return priorErr
		}
		current, readErr := tx.GetRun(tc, req.RunID)
		if readErr != nil {
			return readErr
		}
		if current == nil || current.State != run.StatePaused || current.ID != req.RunID || current.Scope.Canonical() != scope.Canonical() || current.Provenance.OrchestrationID != a2a.RoutingOrchestration(tc) {
			return cortex.ErrNotSuspended
		}
		if current.Revision != req.RunRevision {
			return cortex.ErrRunConflict
		}
		paused, readErr := tx.LockSuspension(tc, req.RunID)
		if readErr != nil {
			return readErr
		}
		at := s.clock.Now().UTC()
		if paused == nil || paused.ID != req.SuspensionID || paused.RunID != req.RunID || paused.Scope.Canonical() != scope.Canonical() || paused.Reason != suspension.ReasonAgentReply || len(paused.Pending) == 0 || len(paused.Pending) > 256 {
			return cortex.ErrNotSuspended
		}
		if paused.ExpiresAt != nil && !paused.ExpiresAt.After(at) {
			return cortex.ErrSuspensionExpired
		}
		ask, readErr := tx.LockPendingAsk(tc, req.ReplyWith)
		if readErr != nil {
			return readErr
		}
		if checkErr := validResolutionAsk(tc, ask); checkErr != nil {
			return checkErr
		}
		if ask.ClaimedAt != nil {
			return a2a.ErrAskAlreadyClaimed
		}
		if ask.Deadline != nil && !ask.Deadline.After(at) {
			return a2a.ErrHeldAskConflict
		}
		matches := 0
		for _, call := range paused.Pending {
			if call.ReplyWith == req.ReplyWith && call.ID == ask.ToolCallID && call.Name == "agent_ask" {
				matches++
			}
		}
		if matches != 1 || ask.AskerRunID != req.RunID {
			return a2a.ErrHeldAskConflict
		}
		message, readErr := tx.ReadMessage(tc, ask.MessageID)
		if readErr != nil {
			return readErr
		}
		if message == nil || message.ID != ask.MessageID {
			return a2a.ErrHeldAskConflict
		}
		class, valid := message.Performative.Class()
		if !valid || class != a2a.ClassDirective || message.Validate() != nil || message.Metadata[a2a.HeldAskMetadata] != "v1" || message.InReplyTo != "" || message.OriginRunID != req.RunID || message.ReplyWith != req.ReplyWith || message.ConversationID != ask.ConversationID || message.Sender.Agent != ask.AskerAgent || message.OrchestrationID != a2a.RoutingOrchestration(tc) || message.Scope.Canonical() != scope.Canonical() || len(message.Receivers) > 256 || !slices.Contains(message.Receivers, ask.Expected) {
			return a2a.ErrHeldAskConflict
		}
		if lockErr := tx.LockConversation(tc, message.ConversationID); lockErr != nil {
			return lockErr
		}
		conversation, readErr := tx.ReadConversation(tc, message.ConversationID)
		if readErr != nil {
			return readErr
		}
		if conversation == nil || !conversation.IsOpen() {
			return a2a.ErrConversationClosed
		}
		seen := make(map[a2a.Address]bool, len(message.Receivers))
		for _, receiver := range message.Receivers {
			if seen[receiver] {
				return a2a.ErrHeldAskConflict
			}
			seen[receiver] = true
		}
		result = a2a.HeldAskReceipt{SchemaVersion: 1, Scope: scope, OrchestrationID: a2a.RoutingOrchestration(tc), Request: req, MessageID: message.ID, CommittedAt: at}
		for _, receiver := range message.Receivers {
			queued := &a2a.Delivery{Entity: cortex.Entity{CreatedAt: at, UpdatedAt: at}, ID: id.NewDeliveryID(), Scope: scope, OrchestrationID: result.OrchestrationID, MessageID: message.ID, Receiver: receiver, State: a2a.DeliveryQueued}
			if saveErr := tx.SaveReplyDelivery(tc, queued); saveErr != nil {
				return saveErr
			}
			result.Deliveries = append(result.Deliveries, queued.ID)
		}
		data, encodeErr := json.Marshal(result)
		if encodeErr != nil {
			return encodeErr
		}
		row, recordErr := record(kindHeldAskRelease, resolutionKey(tc, req.ReplyWith), evidence.Digest(data), resolutionPartition(tc), scope, result)
		if recordErr != nil {
			return recordErr
		}
		if insertErr := tx.Insert(tc, row); insertErr != nil {
			return insertErr
		}
		result.Fresh = true
		return tc.Err()
	})
	if err != nil {
		return a2a.HeldAskReceipt{}, err
	}
	return result, nil
}
