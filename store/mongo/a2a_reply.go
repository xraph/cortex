package mongo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store/internal/assurance"
)

func messagingFilter(ctx context.Context, key id.ID) (bson.M, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	filter := a2aRoutingFilter(ctx, bson.M{"_id": key.String(), "scope_canon": scope.Canonical()})
	for k, v := range scopeFilter(scope, true) {
		filter[k] = v
	}
	return filter, nil
}

func (d assuranceDriver) TransactMessaging(ctx context.Context, delivery id.DeliveryID, conversation id.ConversationID, callback func(context.Context, assurance.MessagingTransaction) error) error {
	if _, err := cortex.RequireScope(ctx); err != nil {
		return err
	}
	session, err := d.store.mdb.Client().StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sc context.Context) (any, error) {
		transactional := assuranceDriver{store: d.store, resultTransaction: true}
		if !delivery.IsNil() {
			if lockErr := transactional.lockMessaging(sc, colA2ADeliveries, delivery, a2a.ErrDeliveryNotFound); lockErr != nil {
				return nil, lockErr
			}
		} else if lockErr := transactional.LockConversation(sc, conversation); lockErr != nil {
			return nil, lockErr
		}
		return true, callback(sc, transactional)
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()))
	if err != nil {
		var commandErr mongo.CommandError
		if errors.As(err, &commandErr) && commandErr.Code == 20 {
			return fmt.Errorf("%w: %w", a2a.ErrReplyCommitUnavailable, err)
		}
	}
	return err
}
func (d assuranceDriver) messagingSession(ctx context.Context) error {
	session := mongo.SessionFromContext(ctx)
	if !d.resultTransaction || session == nil || !session.TransactionRunning() {
		return a2a.ErrReplyCommitUnavailable
	}
	return nil
}
func (d assuranceDriver) lockMessaging(ctx context.Context, collection string, key id.ID, missing error) error {
	if err := d.messagingSession(ctx); err != nil {
		return err
	}
	filter, err := messagingFilter(ctx, key)
	if err != nil {
		return err
	}
	changed, err := d.store.mdb.Collection(collection).UpdateOne(ctx, filter, bson.M{"$inc": bson.M{"reply_fence": int64(1)}})
	if err != nil {
		return err
	}
	if changed.MatchedCount != 1 {
		return missing
	}
	return nil
}
func (d assuranceDriver) LockConversation(ctx context.Context, key id.ConversationID) error {
	return d.lockMessaging(ctx, colA2AConversations, key, a2a.ErrConversationNotFound)
}

func (d assuranceDriver) ReadMessage(ctx context.Context, key id.MessageID) (*a2a.Envelope, error) {
	filter, err := messagingFilter(ctx, key)
	if err != nil {
		return nil, err
	}
	var model a2aMessageModel
	if err = d.store.mdb.Collection(colA2AMessages).FindOne(ctx, filter).Decode(&model); err != nil {
		if isNoDocuments(err) {
			return nil, a2a.ErrMessageNotFound
		}
		return nil, err
	}
	return a2aMessageFromModel(&model)
}

func (d assuranceDriver) ReadConversation(ctx context.Context, key id.ConversationID) (*a2a.Conversation, error) {
	filter, err := messagingFilter(ctx, key)
	if err != nil {
		return nil, err
	}
	var model a2aConversationModel
	if err = d.store.mdb.Collection(colA2AConversations).FindOne(ctx, filter).Decode(&model); err != nil {
		if isNoDocuments(err) {
			return nil, a2a.ErrConversationNotFound
		}
		return nil, err
	}
	return a2aConversationFromModel(&model)
}

func (d assuranceDriver) ReadDelivery(ctx context.Context, key id.DeliveryID) (*a2a.Delivery, error) {
	filter, err := messagingFilter(ctx, key)
	if err != nil {
		return nil, err
	}
	var model a2aDeliveryModel
	if err = d.store.mdb.Collection(colA2ADeliveries).FindOne(ctx, filter).Decode(&model); err != nil {
		if isNoDocuments(err) {
			return nil, a2a.ErrDeliveryNotFound
		}
		return nil, err
	}
	return a2aDeliveryFromModel(&model)
}

func (d assuranceDriver) SaveMessage(ctx context.Context, message *a2a.Envelope, conv *a2a.Conversation) error {
	if err := d.messagingSession(ctx); err != nil {
		return err
	}
	if _, err := d.store.mdb.Collection(colA2AMessages).InsertOne(ctx, a2aMessageToModel(message)); err != nil {
		return err
	}
	filter, err := messagingFilter(ctx, conv.ID)
	if err != nil {
		return err
	}
	changed, err := d.store.mdb.Collection(colA2AConversations).UpdateOne(ctx, filter, bson.M{"$set": bson.M{"participants": conv.Participants, "hops_used": conv.HopsUsed, "updated_at": conv.UpdatedAt}})
	if err != nil {
		return err
	}
	if changed.MatchedCount != 1 {
		return a2a.ErrConversationNotFound
	}
	return nil
}
func (d assuranceDriver) SaveReplyDelivery(ctx context.Context, delivery *a2a.Delivery) error {
	if err := d.messagingSession(ctx); err != nil {
		return err
	}
	_, err := d.store.mdb.Collection(colA2ADeliveries).InsertOne(ctx, a2aDeliveryToModel(delivery))
	return err
}
func (d assuranceDriver) FinishReplyDelivery(ctx context.Context, delivery *a2a.Delivery, at time.Time) error {
	if err := d.messagingSession(ctx); err != nil {
		return err
	}
	filter, err := messagingFilter(ctx, delivery.ID)
	if err != nil {
		return err
	}
	filter["claim_token"], filter["run_id"], filter["state"] = delivery.ClaimToken, delivery.RunID.String(), a2a.DeliveryDelivering
	changed, err := d.store.mdb.Collection(colA2ADeliveries).UpdateOne(ctx, filter, bson.M{"$set": bson.M{"state": a2a.DeliveryDelivered, "delivered_at": at, "updated_at": at}})
	if err != nil {
		return err
	}
	if changed.MatchedCount != 1 {
		return a2a.ErrDeliveryClaimLost
	}
	return nil
}
