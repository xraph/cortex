package mongo

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/store/internal/assurance"
)

func (d assuranceDriver) TransactPlan(ctx context.Context, planID id.ID, callback func(context.Context, assurance.Driver) error) error {
	return d.transactAssuranceRecord(ctx, assurance.KindPlan, planID, planning.ErrNotFound, coordination.ErrAssignmentUnavailable, callback)
}

func (d assuranceDriver) TransactRunCalls(ctx context.Context, runID id.AgentRunID, callback func(context.Context, assurance.Driver) error) error {
	return d.transactAssuranceRecord(ctx, assurance.KindRunCallGuard, runID, execution.ErrInvalidRunCall, execution.ErrRunCallUnavailable, callback)
}

func (d assuranceDriver) transactAssuranceRecord(ctx context.Context, kind string, identifier id.ID, missing, unavailable error, callback func(context.Context, assurance.Driver) error) error {
	filter, err := assuranceFilter(ctx)
	if err != nil {
		return err
	}
	filter["kind"], filter["id"], filter["deleted"] = kind, identifier.String(), false
	session, err := d.store.mdb.Client().StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sc context.Context) (any, error) {
		// A real write fences concurrent revisions even though their immutable rows
		// differ. Snapshot reads alone would permit both transactions to commit.
		result, lockErr := d.store.mdb.Collection(colAssurance).UpdateOne(sc, filter, bson.M{"$inc": bson.M{"coordination_fence": int64(1)}})
		if lockErr != nil {
			return nil, lockErr
		}
		if result.MatchedCount != 1 {
			return nil, missing
		}
		return true, callback(sc, assuranceDriver{store: d.store, resultTransaction: true})
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()))
	if err != nil {
		var commandErr mongo.CommandError
		if errors.As(err, &commandErr) && commandErr.Code == 20 {
			return fmt.Errorf("%w: %w", unavailable, err)
		}
	}
	return err
}
