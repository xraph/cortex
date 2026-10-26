package mongo

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store/internal/replydiscovery"
	"github.com/xraph/cortex/suspension"
)

// ListAgentReplyRuns projects bounded run identities from exact-scope suspensions.
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
	return replydiscovery.Parse(rows, after, limit)
}
