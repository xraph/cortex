package mongo

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/suspension"
)

func (d assuranceDriver) LockSuspension(ctx context.Context, runID id.AgentRunID) (*suspension.Suspension, error) {
	session := mongo.SessionFromContext(ctx)
	if !d.resultTransaction || session == nil || !session.TransactionRunning() {
		return nil, suspension.ErrVersionClaimUnavailable
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	filter := scopeFilter(scope, true)
	filter["run_id"], filter["scope_canon"] = runID.String(), scope.Canonical()
	changed, err := d.store.mdb.Collection(colSuspensions).UpdateOne(ctx, filter, bson.M{"$inc": bson.M{"resume_fence": int64(1)}})
	if err != nil {
		return nil, err
	}
	if changed.MatchedCount != 1 {
		return nil, cortex.ErrNotSuspended
	}
	var model suspensionModel
	if err = d.store.mdb.Collection(colSuspensions).FindOne(ctx, filter).Decode(&model); err != nil {
		if isNoDocuments(err) {
			return nil, cortex.ErrNotSuspended
		}
		return nil, err
	}
	return suspensionFromModel(&model)
}
