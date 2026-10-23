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
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

func (d assuranceDriver) TransactResult(ctx context.Context, _ id.AgentRunID, callback func(context.Context, assurance.ResultTransaction) (verification.CommitReceipt, error)) (verification.CommitReceipt, error) {
	session, err := d.store.mdb.Client().StartSession()
	if err != nil {
		return verification.CommitReceipt{}, err
	}
	defer session.EndSession(ctx)
	var receipt verification.CommitReceipt
	_, err = session.WithTransaction(ctx, func(sc context.Context) (any, error) {
		receipt = verification.CommitReceipt{}
		var callbackErr error
		receipt, callbackErr = callback(sc, assuranceDriver{store: d.store, resultTransaction: true})
		return receipt, callbackErr
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()))
	if err != nil {
		var commandErr mongo.CommandError
		if errors.As(err, &commandErr) && commandErr.Code == 20 {
			return verification.CommitReceipt{}, fmt.Errorf("%w: %w", verification.ErrCommitUnavailable, err)
		}
		return verification.CommitReceipt{}, err
	}
	return receipt, nil
}

func (d assuranceDriver) GetRun(ctx context.Context, runID id.AgentRunID) (*run.Run, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	filter := scopeFilter(scope, true)
	filter["_id"], filter["scope_canon"] = runID.String(), scope.Canonical()
	var m runModel
	if err = d.store.mdb.NewFind(&m).Filter(filter).Scan(ctx); err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrRunNotFound
		}
		return nil, err
	}
	return runFromModel(&m)
}

func (d assuranceDriver) CommitRun(ctx context.Context, runID id.AgentRunID, revision int64, resultID id.ID, at time.Time) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	filter := scopeFilter(scope, true)
	filter["_id"], filter["scope_canon"], filter["revision"] = runID.String(), scope.Canonical(), revision
	result, err := d.store.mdb.NewUpdate((*runModel)(nil)).Filter(filter).SetUpdate(bson.M{"$set": bson.M{"revision": revision + 1, "verification_status": string(verification.StatusPassed), "result_ref": resultID.String(), "updated_at": at}}).Exec(ctx)
	if err != nil {
		return err
	}
	if result.MatchedCount() != 1 {
		return cortex.ErrRunConflict
	}
	return nil
}
