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
	"github.com/xraph/cortex/store/internal/assurance"
)

func resolutionAskFilter(ctx context.Context, token string) (bson.M, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	filter := a2aRoutingFilter(ctx, bson.M{"_id": token, "scope_canon": scope.Canonical()})
	for key, value := range scopeFilter(scope, true) {
		filter[key] = value
	}
	return filter, nil
}

func (d assuranceDriver) TransactAskResolution(ctx context.Context, token string, callback func(context.Context, assurance.AskResolutionTransaction) error) error {
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
		if _, lockErr := transactional.LockPendingAsk(sc, token); lockErr != nil {
			return nil, lockErr
		}
		return true, callback(sc, transactional)
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()))
	if err != nil {
		var commandErr mongo.CommandError
		if errors.As(err, &commandErr) && commandErr.Code == 20 {
			return fmt.Errorf("%w: %w", a2a.ErrAskResolutionUnavailable, err)
		}
	}
	return err
}

func (d assuranceDriver) ReadPendingAsk(ctx context.Context, token string) (*a2a.PendingAsk, error) {
	filter, err := resolutionAskFilter(ctx, token)
	if err != nil {
		return nil, err
	}
	var model a2aPendingAskModel
	if err = d.store.mdb.Collection(colA2APendingAsks).FindOne(ctx, filter).Decode(&model); err != nil {
		if isNoDocuments(err) {
			return nil, a2a.ErrAskNotFound
		}
		return nil, err
	}
	return a2aPendingAskFromModel(&model)
}

func (d assuranceDriver) ClaimResolvedAsk(ctx context.Context, token string, at time.Time) error {
	if d.messagingSession(ctx) != nil {
		return a2a.ErrAskResolutionUnavailable
	}
	filter, err := resolutionAskFilter(ctx, token)
	if err != nil {
		return err
	}
	filter["claimed_at"] = nil
	changed, err := d.store.mdb.Collection(colA2APendingAsks).UpdateOne(ctx, filter, bson.M{"$set": bson.M{"claimed_at": at, "updated_at": at}})
	if err != nil {
		return err
	}
	if changed.MatchedCount != 1 {
		return a2a.ErrAskAlreadyClaimed
	}
	return nil
}

func (d assuranceDriver) LockPendingAsk(ctx context.Context, token string) (*a2a.PendingAsk, error) {
	if err := d.messagingSession(ctx); err != nil {
		return nil, err
	}
	filter, err := resolutionAskFilter(ctx, token)
	if err != nil {
		return nil, err
	}
	changed, err := d.store.mdb.Collection(colA2APendingAsks).UpdateOne(ctx, filter, bson.M{"$inc": bson.M{"resolution_fence": int64(1)}})
	if err != nil {
		return nil, err
	}
	if changed.MatchedCount != 1 {
		return nil, a2a.ErrAskNotFound
	}
	return d.ReadPendingAsk(ctx, token)
}
