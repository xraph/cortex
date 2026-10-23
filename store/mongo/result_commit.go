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
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

func (d assuranceDriver) TransactRun(ctx context.Context, runID id.AgentRunID, callback func(context.Context, assurance.RunTransaction) error) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	session, err := d.store.mdb.Client().StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sc context.Context) (any, error) {
		// A real write serializes run changes with job claims and result commits.
		// Snapshot reads alone do not fence a concurrent update in another document.
		filter := scopeFilter(scope, true)
		filter["_id"], filter["scope_canon"] = runID.String(), scope.Canonical()
		if _, lockErr := d.store.mdb.Collection(colRuns).UpdateOne(sc, filter, bson.M{"$inc": bson.M{"assurance_fence": int64(1)}}); lockErr != nil {
			return nil, lockErr
		}
		return true, callback(sc, assuranceDriver{store: d.store, resultTransaction: true})
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()))
	if err != nil {
		var commandErr mongo.CommandError
		if errors.As(err, &commandErr) && commandErr.Code == 20 {
			return fmt.Errorf("%w: %w", verification.ErrCommitUnavailable, err)
		}
	}
	return err
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

func (d assuranceDriver) SetRunVerification(ctx context.Context, runID id.AgentRunID, revision int64, status verification.Status, resultID id.ID, at time.Time) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	filter := scopeFilter(scope, true)
	filter["_id"], filter["scope_canon"], filter["revision"] = runID.String(), scope.Canonical(), revision
	result, err := d.store.mdb.NewUpdate((*runModel)(nil)).Filter(filter).SetUpdate(bson.M{"$set": bson.M{"revision": revision + 1, "verification_status": string(status), "result_ref": resultID.String(), "updated_at": at}}).Exec(ctx)
	if err != nil {
		return err
	}
	if result.MatchedCount() != 1 {
		return cortex.ErrRunConflict
	}
	return nil
}

func (d assuranceDriver) ReplaceJob(ctx context.Context, row assurance.Record, expectedBody string) error {
	return d.replaceMutableRecord(ctx, row, expectedBody, assurance.KindJob, verification.ErrJobConflict)
}

func (d assuranceDriver) ReplaceEventStream(ctx context.Context, row assurance.Record, expectedBody string) error {
	return d.replaceMutableRecord(ctx, row, expectedBody, assurance.KindEventStream, event.ErrConflict)
}

func (d assuranceDriver) replaceMutableRecord(ctx context.Context, row assurance.Record, expectedBody, kind string, conflict error) error {
	if row.Kind != kind {
		return verification.ErrInvalid
	}
	filter, err := assuranceFilter(ctx)
	if err != nil {
		return err
	}
	filter["kind"], filter["id"], filter["body"], filter["deleted"] = kind, row.ID, expectedBody, false
	result, err := d.store.mdb.Collection(colAssurance).UpdateOne(ctx, filter, bson.M{"$set": bson.M{"body": row.Body, "digest": row.Digest}})
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return conflict
	}
	return nil
}
