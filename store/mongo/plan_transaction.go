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
	if _, err := assuranceFilter(ctx); err != nil {
		return err
	}
	session, err := d.store.mdb.Client().StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sc context.Context) (any, error) {
		transactional := assuranceDriver{store: d.store, resultTransaction: true}
		if lockErr := transactional.lockAssuranceRecord(sc, kind, identifier, missing); lockErr != nil {
			return nil, lockErr
		}
		return true, callback(sc, transactional)
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority()))
	if err != nil {
		var commandErr mongo.CommandError
		if errors.As(err, &commandErr) && commandErr.Code == 20 {
			return fmt.Errorf("%w: %w", unavailable, err)
		}
	}
	return err
}

// LockRunCalls requires the same active session as the enclosing transaction.
func (d assuranceDriver) LockRunCalls(ctx context.Context, runID id.AgentRunID) error {
	session := mongo.SessionFromContext(ctx)
	if !d.resultTransaction || session == nil || !session.TransactionRunning() {
		return execution.ErrRunCallUnavailable
	}
	if runID.IsNil() || runID.Prefix() != id.PrefixAgentRun {
		return execution.ErrInvalidRunCall
	}
	return d.lockAssuranceRecord(ctx, assurance.KindRunCallGuard, runID, execution.ErrInvalidRunCall)
}

func (d assuranceDriver) lockAssuranceRecord(ctx context.Context, kind string, identifier id.ID, missing error) error {
	filter, err := assuranceFilter(ctx)
	if err != nil {
		return err
	}
	filter["kind"], filter["id"], filter["deleted"] = kind, identifier.String(), false
	// A real write fences concurrent revisions even when their immutable rows
	// differ. Snapshot reads alone would permit both transactions to commit.
	result, err := d.store.mdb.Collection(colAssurance).UpdateOne(ctx, filter, bson.M{"$inc": bson.M{"coordination_fence": int64(1)}})
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return missing
	}
	return nil
}
