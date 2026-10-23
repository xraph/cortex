package mongo

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

func (d assuranceDriver) LockSubmissionRun(ctx context.Context, identifier id.AgentRunID) (*run.Run, error) {
	if !d.resultTransaction {
		return nil, coordination.ErrAssignmentUnavailable
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	filter := scopeFilter(scope, true)
	filter["_id"], filter["scope_canon"] = identifier.String(), scope.Canonical()
	result, err := d.store.mdb.Collection(colRuns).UpdateOne(ctx, filter, bson.M{"$inc": bson.M{"assurance_fence": int64(1)}})
	if err != nil {
		return nil, err
	}
	if result.MatchedCount != 1 {
		return nil, cortex.ErrRunNotFound
	}
	return d.GetRun(ctx, identifier)
}
