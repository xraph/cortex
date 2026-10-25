package sqlite

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store/internal/assurance"
)

func (d assuranceDriver) TransactMessaging(ctx context.Context, delivery id.DeliveryID, conversation id.ConversationID, callback func(context.Context, assurance.MessagingTransaction) error) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	tx, err := d.store.sdb.BeginTxQuery(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // Rollback after commit is harmless.
	transactional := assuranceDriver{store: d.store, transaction: tx}
	if !delivery.IsNil() {
		q := tx.NewUpdate((*a2aDeliveryModel)(nil)).Set("updated_at = updated_at").Where("id = ?", delivery.String()).Where("scope_canon = ?", scope.Canonical()).Where("orchestration_id = ?", a2a.RoutingOrchestration(ctx))
		for _, p := range scopePredicates(scope, true) {
			q = q.Where(p.Column+" = ?", p.Value)
		}
		changed, lockErr := q.Exec(ctx)
		if lockErr != nil {
			return lockErr
		}
		count, countErr := changed.RowsAffected()
		if countErr != nil {
			return countErr
		}
		if count != 1 {
			return a2a.ErrDeliveryNotFound
		}
	} else if lockErr := transactional.LockConversation(ctx, conversation); lockErr != nil {
		return lockErr
	}
	if callbackErr := callback(ctx, transactional); callbackErr != nil {
		return callbackErr
	}
	return tx.Commit()
}

func (d assuranceDriver) LockConversation(ctx context.Context, key id.ConversationID) error {
	if d.transaction == nil {
		return a2a.ErrReplyCommitUnavailable
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	q := d.queries().NewUpdate((*a2aConversationModel)(nil)).Set("updated_at = updated_at").Where("id = ?", key.String()).Where("scope_canon = ?", scope.Canonical()).Where("orchestration_id = ?", a2a.RoutingOrchestration(ctx))
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	changed, err := q.Exec(ctx)
	if err != nil {
		return err
	}
	count, err := changed.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return a2a.ErrConversationNotFound
	}
	return nil
}

func (d assuranceDriver) ReadMessage(ctx context.Context, key id.MessageID) (*a2a.Envelope, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	model := new(a2aMessageModel)
	q := d.queries().NewSelect(model).Where("id = ?", key.String()).Where("scope_canon = ?", scope.Canonical()).Where("orchestration_id = ?", a2a.RoutingOrchestration(ctx))
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err = q.Scan(ctx); err != nil {
		if isNoRows(err) {
			return nil, a2a.ErrMessageNotFound
		}
		return nil, err
	}
	return a2aMessageFromModel(model)
}

func (d assuranceDriver) ReadConversation(ctx context.Context, key id.ConversationID) (*a2a.Conversation, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	model := new(a2aConversationModel)
	q := d.queries().NewSelect(model).Where("id = ?", key.String()).Where("scope_canon = ?", scope.Canonical()).Where("orchestration_id = ?", a2a.RoutingOrchestration(ctx))
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err = q.Scan(ctx); err != nil {
		if isNoRows(err) {
			return nil, a2a.ErrConversationNotFound
		}
		return nil, err
	}
	return a2aConversationFromModel(model)
}

func (d assuranceDriver) ReadDelivery(ctx context.Context, key id.DeliveryID) (*a2a.Delivery, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	model := new(a2aDeliveryModel)
	q := d.queries().NewSelect(model).Where("id = ?", key.String()).Where("scope_canon = ?", scope.Canonical()).Where("orchestration_id = ?", a2a.RoutingOrchestration(ctx))
	for _, p := range scopePredicates(scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	if err = q.Scan(ctx); err != nil {
		if isNoRows(err) {
			return nil, a2a.ErrDeliveryNotFound
		}
		return nil, err
	}
	return a2aDeliveryFromModel(model)
}

func (d assuranceDriver) SaveMessage(ctx context.Context, message *a2a.Envelope, conv *a2a.Conversation) error {
	if d.transaction == nil {
		return a2a.ErrReplyCommitUnavailable
	}
	if _, err := d.queries().NewInsert(a2aMessageToModel(message)).Exec(ctx); err != nil {
		return err
	}
	q := d.queries().NewUpdate(a2aConversationToModel(conv)).Column("participants", "hops_used", "updated_at").Where("id = ?", conv.ID.String()).Where("scope_canon = ?", conv.Scope.Canonical()).Where("orchestration_id = ?", a2a.RoutingOrchestration(ctx))
	for _, p := range scopePredicates(conv.Scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	changed, err := q.Exec(ctx)
	if err != nil {
		return err
	}
	count, err := changed.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return a2a.ErrConversationNotFound
	}
	return nil
}
func (d assuranceDriver) SaveReplyDelivery(ctx context.Context, delivery *a2a.Delivery) error {
	if d.transaction == nil {
		return a2a.ErrReplyCommitUnavailable
	}
	_, err := d.queries().NewInsert(a2aDeliveryToModel(delivery)).Exec(ctx)
	return err
}
func (d assuranceDriver) FinishReplyDelivery(ctx context.Context, delivery *a2a.Delivery, at time.Time) error {
	if d.transaction == nil {
		return a2a.ErrReplyCommitUnavailable
	}
	q := d.queries().NewUpdate((*a2aDeliveryModel)(nil)).Set("state = ?", a2a.DeliveryDelivered).Set("delivered_at = ?", at).Set("updated_at = ?", at).Where("id = ?", delivery.ID.String()).Where("scope_canon = ?", delivery.Scope.Canonical()).Where("orchestration_id = ?", a2a.RoutingOrchestration(ctx)).Where("claim_token = ?", delivery.ClaimToken).Where("run_id = ?", delivery.RunID.String()).Where("state = ?", a2a.DeliveryDelivering)
	for _, p := range scopePredicates(delivery.Scope, true) {
		q = q.Where(p.Column+" = ?", p.Value)
	}
	changed, err := q.Exec(ctx)
	if err != nil {
		return err
	}
	count, err := changed.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return a2a.ErrDeliveryClaimLost
	}
	return nil
}
