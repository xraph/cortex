package mongo

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/internal/replydiscovery"
	"github.com/xraph/cortex/suspension"
)

// ListAgentReplyRuns includes saved pauses and running admissions whose pause was removed.
func (s *Store) ListAgentReplyRuns(ctx context.Context, after id.AgentRunID, limit int) ([]id.AgentRunID, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	if validationErr := replydiscovery.Validate(after, limit); validationErr != nil {
		return nil, validationErr
	}
	filter := scopeFilter(scope, true)
	filter["scope_canon"], filter["reason"] = scope.Canonical(), string(suspension.ReasonAgentReply)
	if !after.IsNil() {
		filter["run_id"] = bson.M{"$gt": after.String()}
	}
	cursor, err := s.mdb.Collection(colSuspensions).Find(ctx, filter, options.Find().SetProjection(bson.M{"run_id": 1, "_id": 0}).SetSort(bson.D{{Key: "run_id", Value: 1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var models []struct {
		RunID string `bson:"run_id"`
	}
	if scanErr := cursor.All(ctx, &models); scanErr != nil {
		return nil, scanErr
	}
	rows := make([]string, 0, len(models))
	for _, model := range models {
		rows = append(rows, model.RunID)
	}
	activeFilter := scopeFilter(scope, true)
	activeFilter["scope_canon"], activeFilter["state"] = scope.Canonical(), string(run.StateRunning)
	if !after.IsNil() {
		activeFilter["_id"] = bson.M{"$gt": after.String()}
	}
	admissionFilter := scopeFilter(scope, true)
	admissionFilter["scope_canon"], admissionFilter["kind"], admissionFilter["deleted"] = scope.Canonical(), "resume_admission", false
	admissionFilter["$expr"] = bson.M{"$eq": bson.A{"$run_id", "$$run"}}
	pipeline := bson.A{
		bson.M{"$match": activeFilter},
		bson.M{"$sort": bson.M{"_id": 1}},
		bson.M{"$lookup": bson.M{"from": colAssurance, "let": bson.M{"run": "$_id"}, "pipeline": bson.A{bson.M{"$match": admissionFilter}, bson.M{"$limit": 1}, bson.M{"$project": bson.M{"_id": 1}}}, "as": "admissions"}},
		bson.M{"$match": bson.M{"admissions.0": bson.M{"$exists": true}}},
		bson.M{"$limit": int64(limit)},
		bson.M{"$project": bson.M{"_id": 1}},
	}
	active, err := s.mdb.Collection(colRuns).Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	defer active.Close(ctx)
	var admitted []struct {
		ID string `bson:"_id"`
	}
	if scanErr := active.All(ctx, &admitted); scanErr != nil {
		return nil, scanErr
	}
	activeRows := make([]string, 0, len(admitted))
	for _, model := range admitted {
		activeRows = append(activeRows, model.ID)
	}
	return replydiscovery.Merge(rows, activeRows, after, limit)
}
