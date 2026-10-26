package mongo

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store/internal/recipientdiscovery"
)

// ListDeferredRecipients projects bound pending delivery IDs in the exact scope.
func (s *Store) ListDeferredRecipients(ctx context.Context, after id.DeliveryID, limit int) ([]id.DeliveryID, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	if validationErr := recipientdiscovery.Validate(after, limit); validationErr != nil {
		return nil, validationErr
	}
	filter := a2aRoutingFilter(ctx, scopeFilter(scope, true))
	filter["scope_canon"], filter["state"] = scope.Canonical(), a2a.DeliveryDelivering
	filter["run_id"] = bson.M{"$type": "string", "$ne": ""}
	filter["claim_token"] = bson.M{"$type": "string", "$ne": ""}
	if !after.IsNil() {
		filter["_id"] = bson.M{"$gt": after.String()}
	}
	cursor, err := s.mdb.Collection(colA2ADeliveries).Find(ctx, filter, options.Find().SetProjection(bson.M{"_id": 1}).SetSort(bson.D{{Key: "_id", Value: 1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var models []struct {
		ID string `bson:"_id"`
	}
	if scanErr := cursor.All(ctx, &models); scanErr != nil {
		return nil, scanErr
	}
	rows := make([]string, 0, len(models))
	for _, model := range models {
		rows = append(rows, model.ID)
	}
	return recipientdiscovery.Parse(rows, after, limit)
}
